export const meta = { name: 'seq' }
const r = await parallel([3, 2, 1].map(n => () => agent('p' + n, { label: 'l' + n, kind: 'claude' })))
await agent('last', { label: 'l1' })
return r
