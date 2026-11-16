// A small review cycle: write, review, validate findings in parallel, fix,
// and go around again until no finding holds up.
//   sheepdog graph run examples/review-cycle.ts "sheepdogs"
export const meta = { name: 'review-cycle' }

const FINDINGS = {
  type: 'object',
  properties: {
    findings: {
      type: 'array',
      items: {
        type: 'object',
        properties: { id: { type: 'string' }, title: { type: 'string' } },
        required: ['id', 'title'],
      },
    },
  },
  required: ['findings'],
}
const VERDICT = {
  type: 'object',
  properties: { id: { type: 'string' }, isReal: { type: 'boolean' } },
  required: ['id', 'isReal'],
}

await agent(`Write notes.md: three short sentences about ${args.instruction}. Then answer "done".`, {
  label: 'implement', kind: 'claude',
})

let rounds = 0
for (let round = 0; round < 3; round++) {
  rounds++
  const review = await agent<{ findings: { id: string; title: string }[] }>(
    'Review notes.md. Report exactly two findings, with ids F1 and F2: one real problem and one nitpick.',
    { label: 'review', kind: 'claude', schema: FINDINGS })

  const verdicts = await parallel(review.findings.map(f => () =>
    agent<{ id: string; isReal: boolean }>(
      `Read notes.md. Is this finding a real problem worth fixing? ${f.id}: ${f.title}`,
      { label: `validate-${f.id}`, kind: 'claude', schema: VERDICT })))

  const confirmed = verdicts.filter(v => v.isReal)
  log(`round ${round + 1}: ${confirmed.length} confirmed`)
  if (confirmed.length === 0) break

  await agent(`Fix these findings in notes.md: ${JSON.stringify(confirmed)}. Then answer "fixed".`, {
    label: 'implement',
  })
}
return { rounds }
