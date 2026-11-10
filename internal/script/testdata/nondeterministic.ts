export const meta = { name: 'nondeterministic', description: 'uses the clock' }
return args.which === 'random' ? Math.random() : args.which === 'now' ? Date.now() : new Date().toString()
