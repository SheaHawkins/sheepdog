export const meta = { name: 'fan' }

const rs = await parallel(['a', 'b', 'c'].map(x => () =>
  agent<string>(`check ${x}`, { label: `check-${x}`, kind: 'claude' })))
const s = await agent<string>(`sum ${rs.join(',')}`, { label: 'sum', kind: 'claude' })
return s
