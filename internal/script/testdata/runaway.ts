export const meta = { name: 'runaway', description: 'never stops', maxCalls: 5 }
while (true) {
  await agent('again', { label: 'loop', kind: 'claude' })
}
