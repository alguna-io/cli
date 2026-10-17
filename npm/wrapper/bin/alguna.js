#!/usr/bin/env node
"use strict";

// npm installs only the optionalDependency whose `os` and `cpu` match this
// machine; that package holds the native binary this script runs.
const { spawnSync } = require("node:child_process");

const target = `${process.platform}-${process.arch}`;
const pkg = `@alguna/cli-${target}`;
const exe = process.platform === "win32" ? "alguna.exe" : "alguna";

let binary;
try {
  binary = require.resolve(`${pkg}/bin/${exe}`);
} catch {
  console.error(
    `alguna: ${pkg} is not installed. Either there is no alguna build for ${target}, ` +
      "or npm skipped optional dependencies (--omit=optional or --no-optional). Reinstall @alguna/cli with optional dependencies.",
  );
  process.exit(1);
}

const result = spawnSync(binary, process.argv.slice(2), { stdio: "inherit" });
if (result.error) {
  console.error(`alguna: ${result.error.message}`);
  process.exit(1);
}
if (result.signal) {
  process.kill(process.pid, result.signal);
}
process.exit(result.status ?? 1);
