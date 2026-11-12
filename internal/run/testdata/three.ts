export const meta = { name: 'three' }

const a = await agent<string>('one', { label: 'worker', kind: 'claude' })
const b = await agent<string>(`two after ${a}`, { label: 'worker' })
const c = await agent<string>(`three after ${b}`, { label: 'worker' })
return [a, b, c]
