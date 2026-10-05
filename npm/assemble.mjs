// Builds the npm packages of the CLI from a goreleaser run, in the pattern
// @openai/codex uses: everything is published under the one name @alguna/cli.
// Each platform binary is a version of it (`<version>-<os>-<cpu>`, with `os` and
// `cpu` so npm installs only the matching one) under its own dist-tag. The wrapper
// is `<version>` under `latest` and depends on the platform versions through npm
// aliases (`@alguna/cli-<os>-<cpu>` -> `npm:@alguna/cli@<version>-<os>-<cpu>`).
// One package name means one trusted publisher on npm.
//
// Usage (from the repo root, after goreleaser wrote dist/): node npm/assemble.mjs <outdir>
// Prints "<package dir>\t<dist-tag>" per package in publish order: platform
// versions first, so the wrapper never points at a version that is not on the registry.
import { chmodSync, copyFileSync, cpSync, existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const outDir = process.argv[2];
if (!outDir) {
  console.error("usage: node npm/assemble.mjs <outdir>");
  process.exit(1);
}
if (existsSync(outDir)) {
  console.error(`${outDir} already exists`);
  process.exit(1);
}

// goreleaser GOOS/GOARCH -> Node's process.platform/process.arch, which npm matches `os`/`cpu` against.
const npmOs = { darwin: "darwin", linux: "linux", windows: "win32" };
const npmCpu = { amd64: "x64", arm64: "arm64" };

const wrapper = JSON.parse(readFileSync("npm/wrapper/package.json", "utf8"));
const binaries = JSON.parse(readFileSync("dist/artifacts.json", "utf8"))
  .filter((artifact) => artifact.type === "Binary")
  .sort((a, b) => `${a.goos}-${a.goarch}`.localeCompare(`${b.goos}-${b.goarch}`));

const writeJson = (path, value) => writeFileSync(path, `${JSON.stringify(value, null, 2)}\n`);
const dirs = [];
const optionalDependencies = {};

for (const artifact of binaries) {
  const os = npmOs[artifact.goos];
  const cpu = npmCpu[artifact.goarch];
  if (!os || !cpu) {
    console.error(`no npm platform for ${artifact.goos}/${artifact.goarch}: add it to npm/assemble.mjs`);
    process.exit(1);
  }
  const platform = `${os}-${cpu}`;
  const version = `${wrapper.version}-${platform}`;
  const dir = join(outDir, platform);
  const exe = os === "win32" ? "alguna.exe" : "alguna";
  mkdirSync(join(dir, "bin"), { recursive: true });
  copyFileSync(artifact.path, join(dir, "bin", exe));
  chmodSync(join(dir, "bin", exe), 0o755);
  copyFileSync("LICENSE", join(dir, "LICENSE"));
  writeJson(join(dir, "package.json"), {
    name: wrapper.name,
    version,
    description: `The ${platform} binary of ${wrapper.name}`,
    license: wrapper.license,
    homepage: wrapper.homepage,
    repository: wrapper.repository,
    os: [os],
    cpu: [cpu],
    files: ["bin", "LICENSE"],
    preferUnplugged: true,
    publishConfig: wrapper.publishConfig,
  });
  optionalDependencies[`${wrapper.name}-${platform}`] = `npm:${wrapper.name}@${version}`;
  dirs.push(`${dir}\t${platform}`);
}

const wrapperDir = join(outDir, "wrapper");
cpSync("npm/wrapper", wrapperDir, { recursive: true });
chmodSync(join(wrapperDir, "bin", "alguna.js"), 0o755);
copyFileSync("README.md", join(wrapperDir, "README.md"));
copyFileSync("LICENSE", join(wrapperDir, "LICENSE"));
writeJson(join(wrapperDir, "package.json"), { ...wrapper, optionalDependencies });
dirs.push(`${wrapperDir}\tlatest`);

console.log(dirs.join("\n"));
