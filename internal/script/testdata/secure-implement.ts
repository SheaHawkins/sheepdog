// The example graph from SPEC.md, with its schemas imported from a sibling
// file so relative-import bundling is exercised too.
import { FINDINGS, VERDICT } from './schemas'

export const meta = {
  name: 'secure-implement',
  description: 'Implement, security review, validate findings, fix',
  loop: { every: '6h', instruction: 'Pick up the next open ticket.' },
}

type Finding = { id: string; title: string }
type Verdict = { id: string; isReal: boolean; reason: string }

await agent(`Implement: ${args.instruction}`, {
  label: 'implement', kind: 'claude',
})

let rounds = 0
for (let round = 0; round < 3; round++) {
  rounds++
  const review = await agent<{ findings: Finding[] }>('Security review the diff.', {
    label: 'security', kind: 'codex', schema: FINDINGS,
  })

  const verdicts = await parallel(review.findings.map(f => () =>
    agent<Verdict>(`Try to disprove: ${f.title}`, {
      label: `validate-${f.id}`, kind: 'claude', schema: VERDICT,
    })))

  const confirmed = verdicts.filter(v => v.isReal)
  log(`round ${round}: ${confirmed.length} confirmed`)
  if (confirmed.length === 0) break

  await agent(`Fix these confirmed findings: ${JSON.stringify(confirmed)}`, {
    label: 'implement',
  })
}

return { rounds }
