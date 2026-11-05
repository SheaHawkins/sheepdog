#!/usr/bin/env node
// node:sqlite and stripTypeScriptTypes are still flagged experimental; silence that one warning class.
const emit = process.emitWarning;
process.emitWarning = (warning, ...rest) => {
  const type = typeof rest[0] === "string" ? rest[0] : rest[0]?.type;
  if (type === "ExperimentalWarning") return;
  return emit.call(process, warning, ...rest);
};
const { main } = await import("../src/cli.ts");
process.exitCode = await main(process.argv.slice(2));
