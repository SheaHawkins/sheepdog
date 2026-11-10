export const meta = { name: 'errors', description: "has a } in a string and `a ${'template'}`" }

try {
  await agent('this one fails', { label: 'flaky', kind: 'claude' })
} catch (e) {
  log(`caught: ${(e as Error).message}`)
}

const n: number = 1
if (n === 1) {
  throw new Error('boom from line 11')
}
