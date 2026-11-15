// The smoke graph: a plain-text call, a schema call, parallel() across two
// labels and a relative import, enough to exercise each part of a run.
import { ECHO } from './schemas'

export const meta = { name: 'smoke' }

const plan = await agent<string>(`plan ${args.instruction}`, { label: 'planner', kind: 'claude' })
const checked = await parallel(['a', 'b'].map(x => () =>
  agent<{ echo: string }>(`check ${x}`, { label: `check-${x}`, kind: 'codex', schema: ECHO })))
return { plan, checked: checked.map(c => c.echo), who: args.who }
