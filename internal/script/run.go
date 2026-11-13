package script

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dop251/goja"
)

// AgentOptions are the options a script passes to agent().
type AgentOptions struct {
	Label   string         `json:"label"`
	Kind    string         `json:"kind"`
	Schema  map[string]any `json:"schema"`
	Timeout string         `json:"timeout"`

	// Seq is the call's position in the run, 1-based, in the order the script
	// made its calls. The script is deterministic, so a replay numbers its
	// calls the same way even when parallel() calls finish in another order.
	// Set by the runtime, never by the script.
	Seq int `json:"-"`
}

// Host is what a running script calls out to. Agent blocks until the node
// has produced its result; the runtime calls it from its own goroutine, so
// several calls may be in flight at once when the script uses parallel().
type Host interface {
	Agent(ctx context.Context, prompt string, opts AgentOptions) (any, error)
	Log(message string)
}

// DefaultMaxCalls is the cap on agent() calls when meta.maxCalls is unset.
const DefaultMaxCalls = 50

// Meta evaluates the program's meta declaration without running the graph.
func Meta(p *Program) (map[string]any, error) {
	_, exports, err := load(p)
	if err != nil {
		return nil, err
	}
	m, ok := exports.Get("meta").Export().(map[string]any)
	if !ok {
		return nil, errors.New("meta is not an object")
	}
	return m, nil
}

// Run executes the graph with the given args and returns the value its
// script returns (nil if it returns nothing).
func Run(ctx context.Context, p *Program, args map[string]any, host Host) (any, error) {
	rt, exports, err := load(p)
	if err != nil {
		return nil, err
	}
	maxCalls := DefaultMaxCalls
	if meta, ok := exports.Get("meta").Export().(map[string]any); ok {
		if n, ok := meta["maxCalls"].(int64); ok && n > 0 {
			maxCalls = int(n)
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Each finished agent() call sends a closure here; the loop below runs
	// it on the JS goroutine, which is the only one allowed to touch rt.
	settle := make(chan func() error)
	pending, calls := 0, 0

	agent := func(call goja.FunctionCall) goja.Value {
		promise, resolve, reject := rt.NewPromise()
		prompt := call.Argument(0).String()
		var opts AgentOptions
		if o := call.Argument(1); !goja.IsUndefined(o) && !goja.IsNull(o) {
			// Round-trip through JSON so nested schema objects arrive as
			// plain Go maps.
			raw, err := json.Marshal(o.Export())
			if err == nil {
				err = json.Unmarshal(raw, &opts)
			}
			if err != nil {
				reject(rt.NewTypeError("agent(): bad options: %v", err))
				return rt.ToValue(promise)
			}
		}
		calls++
		if calls > maxCalls {
			reject(rt.NewGoError(fmt.Errorf("run exceeded maxCalls (%d)", maxCalls)))
			return rt.ToValue(promise)
		}
		if opts.Label == "" {
			reject(rt.NewTypeError("agent(): label is required"))
			return rt.ToValue(promise)
		}
		opts.Seq = calls
		pending++
		go func() {
			result, err := host.Agent(ctx, prompt, opts)
			var raw []byte
			if err == nil {
				raw, err = json.Marshal(result)
			}
			select {
			case settle <- func() error {
				if err != nil {
					return reject(rt.NewGoError(err))
				}
				// JSON.parse gives the script real arrays and objects rather
				// than wrapped Go values.
				v, perr := jsonParse(rt, raw)
				if perr != nil {
					return reject(rt.NewGoError(perr))
				}
				return resolve(v)
			}:
			case <-ctx.Done():
			}
		}()
		return rt.ToValue(promise)
	}

	if err := rt.Set("agent", agent); err != nil {
		return nil, err
	}
	if err := rt.Set("log", func(msg string) { host.Log(msg) }); err != nil {
		return nil, err
	}
	if args == nil {
		args = map[string]any{}
	}
	if err := rt.Set("args", args); err != nil {
		return nil, err
	}

	main, ok := goja.AssertFunction(exports.Get("default"))
	if !ok {
		return nil, errors.New("compiled graph has no main function")
	}
	v, err := main(goja.Undefined())
	if err != nil {
		return nil, scriptError(err)
	}
	promise, ok := v.Export().(*goja.Promise)
	if !ok {
		return nil, errors.New("graph main did not return a promise")
	}

	for promise.State() == goja.PromiseStatePending {
		if pending == 0 {
			return nil, errors.New("graph script is waiting on something that will never finish (no agent() call is in flight)")
		}
		select {
		case f := <-settle:
			pending--
			if err := f(); err != nil {
				return nil, scriptError(err)
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if promise.State() == goja.PromiseStateRejected {
		return nil, scriptError(rejection(promise.Result()))
	}
	return promise.Result().Export(), nil
}

// load creates a runtime with the sandbox prelude and evaluates the bundle,
// returning its exports object.
func load(p *Program) (*goja.Runtime, *goja.Object, error) {
	rt := goja.New()
	if _, err := rt.RunString(prelude); err != nil {
		return nil, nil, fmt.Errorf("prelude: %w", err)
	}
	prog, err := goja.Compile(p.Path, p.Code, true)
	if err != nil {
		return nil, nil, err
	}
	if _, err := rt.RunProgram(prog); err != nil {
		return nil, nil, scriptError(err)
	}
	exports := rt.Get(globalName)
	if exports == nil || goja.IsUndefined(exports) {
		return nil, nil, errors.New("compiled graph has no exports")
	}
	return rt, exports.ToObject(rt), nil
}

func jsonParse(rt *goja.Runtime, raw []byte) (goja.Value, error) {
	parse, _ := goja.AssertFunction(rt.Get("JSON").ToObject(rt).Get("parse"))
	return parse(goja.Undefined(), rt.ToValue(string(raw)))
}

// rejection turns a rejected promise's reason into an error, keeping the
// stack trace when the reason is a JS Error.
func rejection(reason goja.Value) error {
	if obj, ok := reason.(*goja.Object); ok {
		if stack := obj.Get("stack"); stack != nil && !goja.IsUndefined(stack) {
			return errors.New(stack.String())
		}
	}
	return errors.New(reason.String())
}

func scriptError(err error) error {
	var ex *goja.Exception
	if errors.As(err, &ex) {
		return errors.New(ex.String())
	}
	return err
}

// prelude runs before the graph. parallel() is plain JavaScript: because
// agent() returns a promise immediately, Promise.all is enough to run calls
// concurrently. Sources of nondeterminism are removed, since resume works
// by replaying the script.
const prelude = `
globalThis.parallel = function parallel(thunks) {
  return Promise.all(thunks.map(function (t) { return t(); }));
};

(function () {
  function unavailable(what) {
    return function () {
      throw new Error(what + " is unavailable: graph scripts must be deterministic so a run can resume by replay");
    };
  }
  Math.random = unavailable("Math.random");
  var RealDate = Date;
  var SheepdogDate = function Date() {
    if (!new.target) unavailable("Date()")();
    if (arguments.length === 0) unavailable("new Date() without arguments")();
    return Reflect.construct(RealDate, arguments, new.target);
  };
  SheepdogDate.prototype = RealDate.prototype;
  SheepdogDate.parse = RealDate.parse;
  SheepdogDate.UTC = RealDate.UTC;
  SheepdogDate.now = unavailable("Date.now");
  globalThis.Date = SheepdogDate;
})();
`
