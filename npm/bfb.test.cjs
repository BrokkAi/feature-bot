const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { spawnSync, spawn } = require("node:child_process");
const { test } = require("node:test");
const { packageName } = require("./bfb.cjs");

test("platform packages and unsupported platforms", () => {
  for (const platform of ["linux", "darwin"]) {
    for (const arch of ["x64", "arm64"]) {
      assert.equal(packageName(platform, arch), `@brokkai/feature-bot-${platform}-${arch}`);
    }
  }
  assert.throws(() => packageName("win32", "x64"), /does not support/);
  assert.throws(() => packageName("linux", "ia32"), /does not support/);
});

function fixture(t, source) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "bfb-npm-"));
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  const launcher = path.join(directory, "bfb.cjs");
  fs.copyFileSync(path.join(__dirname, "bfb.cjs"), launcher);
  const binary = path.join(directory, "node_modules", packageName(), "bin", "bfb");
  fs.mkdirSync(path.dirname(binary), { recursive: true });
  fs.writeFileSync(binary, `#!${process.execPath}\n${source}`, { mode: 0o755 });
  return launcher;
}

test("forwards arguments, working directory, environment, and exit code", (t) => {
  const launcher = fixture(t, `require("node:fs").writeSync(1, JSON.stringify({args: process.argv.slice(2), cwd: process.cwd(), value: process.env.BIB_TEST})); process.exitCode = 23;`);
  const result = spawnSync(process.execPath, [launcher, "--once", "repo with spaces"], {
    encoding: "utf8", env: { ...process.env, BIB_TEST: "preserved" },
  });
  assert.equal(result.status, 23);
  assert.deepEqual(JSON.parse(result.stdout), { args: ["--once", "repo with spaces"], cwd: process.cwd(), value: "preserved" });
});

test("missing native dependency explains how to repair the install", (t) => {
  const launcher = fixture(t, "");
  fs.rmSync(path.join(path.dirname(launcher), "node_modules"), { recursive: true });
  const result = spawnSync(process.execPath, [launcher, "--help"], { encoding: "utf8" });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /optional dependencies enabled/);
});

test("forwards termination to the native process", { timeout: 5000 }, async (t) => {
  const launcher = fixture(t, `process.on("SIGTERM", () => process.exit(42)); require("node:fs").writeSync(1, "ready"); setInterval(() => {}, 1000);`);
  const child = spawn(process.execPath, [launcher], { stdio: ["ignore", "pipe", "pipe"] });
  t.after(() => child.kill("SIGKILL"));
  const closed = new Promise((resolve) => child.on("close", (code) => resolve(code)));
  await new Promise((resolve, reject) => {
    child.once("error", reject);
    child.stdout.once("data", resolve);
  });
  child.kill("SIGTERM");
  assert.equal(await closed, 42);
});
