export const meta = { name: 'three' }

let a = ''
try {
  a = await agent<string>('one, but different', { label: 'worker', kind: 'claude' })
} catch (e) {
  a = 'caught'
}
return await agent<string>(`two after ${a}`, { label: 'worker' })
