/// <reference path="../types/sheepdog.d.ts" />
export const meta = {
  name: "review-and-verify",
  description: "Review the current diff on several dimensions, then adversarially verify each finding",
  phases: [{ title: "Review" }, { title: "Verify" }],
  agents: {
    reviewer: "claude | codex | *",
    verifier: "codex | claude | *",
  },
};

const dims: string[] = args?.dims ?? ["bugs", "performance"];

const Findings = schema({
  findings: [{ file: "string", line: "integer", title: "string", detail: "string" }],
});
const Verdict = schema({ isReal: "boolean", reason: "string" });

const results = await pipeline(
  dims,
  (dim: string) =>
    agent(
      `Review the uncommitted changes in this repository (git diff HEAD) for ${dim} issues.\n` +
        `Report only concrete, specific problems you can point to; an empty list is fine.`,
      { as: "reviewer", schema: Findings, phase: "Review", label: `review: ${dim}` },
    ),
  (review: any) =>
    parallel(
      review.findings.map((f: any) => () =>
        agent(
          `A reviewer claims this problem in ${f.file}:${f.line}:\n\n${f.title}\n${f.detail}\n\n` +
            `Try hard to prove the claim wrong by reading the code. Decide whether it is a real problem.`,
          { as: "verifier", schema: Verdict, phase: "Verify", label: `verify: ${f.title}` },
        ).then((v: any) => ({ ...f, verdict: v })),
      ),
    ),
);

const all = results.flat();
return {
  confirmed: all.filter((f: any) => f.verdict.isReal),
  rejected: all.filter((f: any) => !f.verdict.isReal).length,
};
