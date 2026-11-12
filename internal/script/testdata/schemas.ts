export const FINDINGS = {
  type: 'object',
  required: ['findings'],
  properties: { findings: { type: 'array' } },
}

export const VERDICT = {
  type: 'object',
  required: ['id', 'isReal', 'reason'],
}
