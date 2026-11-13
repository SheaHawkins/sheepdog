export const meta = { name: 'kill' }

const a = await agent<string>('Answer with only the word ONE.', { label: 'worker', kind: 'claude' })
const b = await agent<string>('Using your file-writing tool, write the numbers one to one hundred and fifty as English words, one per line, to count.txt. Then answer with only the word TWO.', { label: 'worker' })
const c = await agent<string>('Answer with only the word THREE.', { label: 'worker' })
return [a, b, c]
