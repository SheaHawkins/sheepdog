// Two calls to one claude agent: a plain-text answer, then a schema result.
//   sheepdog graph run examples/hello.ts PELICAN
export const meta = { name: 'hello' }

const a = await agent<string>(`Answer with only the word ${args.instruction}.`, { label: 'worker', kind: 'claude' })
const b = await agent<{ word: string }>('What word did you just answer? Reply in the result file.', {
  label: 'worker',
  schema: { type: 'object', properties: { word: { type: 'string' } }, required: ['word'] },
})
return { a, b }
