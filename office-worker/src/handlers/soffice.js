// Shared soffice-invocation helper for the three LibreOffice-backed ops
// (xlsx.recalc, pptx.thumbnail, office.convert) — every soffice call must go
// through runSoffice so the per-job profile (and its macro hardening) can
// never be forgotten at a new call site.
//
// Macro hardening: verified empirically
// against LibreOffice 7.4.7.2 (via the office-worker Docker image, since
// soffice isn't installed on every dev host) that `soffice --convert-to`
// batch conversion already refuses to execute a document's embedded macro
// unconditionally — a macro-bearing fixture (test/fixtures/macro.odt, an
// "AutoOpen"-convention StarBasic macro) converts with its body text
// untouched regardless of the profile's MacroSecurityLevel. That built-in
// refusal isn't a documented, versioned contract, so this helper does not
// rely on it alone: every job gets its own disposable UserInstallation
// profile (`-env:UserInstallation`, isolated from any shared/default
// profile — never the container's real $HOME) seeded with a
// registrymodifications.xcu that pins MacroSecurityLevel to 3 ("Very
// high" — no macro executes, not even a signed one, and headless has no UI
// to show a confirmation dialog anyway). Belt-and-suspenders: if a future
// soffice version, a different load path, or the VBA/OOXML macro subsystem
// (untested here — legacy .doc/.xlsm carry VBA, not StarBasic) ever behaves
// differently, the explicit setting is what stops it.
//
// The same profile also forces Calc formula recalculation on load
// (OOXMLRecalcMode/ODFRecalcMode = 2, "always recalculate") — the mechanism
// xlsx.recalc depends on (verified: a workbook with `=A1/A2` where A2=0
// round-trips through `--convert-to xlsx` under this profile with the cached
// value rewritten to the literal string "#DIV/0!"). Harmless no-op for the
// other two ops.
import { mkdir, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { runCommand } from './run-script.js';

const SOFFICE_BIN = process.env.SOFFICE_BIN || 'soffice';

const REGISTRY_XML = `<?xml version="1.0" encoding="UTF-8"?>
<oor:registry xmlns:oor="http://openoffice.org/2001/registry" xmlns:xs="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
 <item oor:path="/org.openoffice.Office.Common/Security/Scripting">
  <prop oor:name="MacroSecurityLevel" oor:op="fuse"><value>3</value></prop>
 </item>
 <item oor:path="/org.openoffice.Office.Calc/Formula/Load">
  <prop oor:name="OOXMLRecalcMode" oor:op="fuse"><value>2</value></prop>
  <prop oor:name="ODFRecalcMode" oor:op="fuse"><value>2</value></prop>
 </item>
</oor:registry>
`;

// Seeds a fresh per-job LibreOffice profile under jobDir (wiped with the
// rest of jobDir at job end — never reused across jobs) and returns the
// -env:UserInstallation arg pointing soffice at it instead of $HOME.
async function prepareProfile(jobDir) {
  const profileDir = join(jobDir, 'loprofile');
  const userDir = join(profileDir, 'user');
  await mkdir(userDir, { recursive: true });
  await writeFile(join(userDir, 'registrymodifications.xcu'), REGISTRY_XML, 'utf8');
  return `-env:UserInstallation=file://${profileDir}`;
}

// Runs soffice --headless under the per-job hardened profile. extraArgs is
// the op-specific tail of the argv (e.g. ['--convert-to', 'pdf', '--outdir',
// outDir, inputPath]) — callers must use a distinct --outdir from the input
// file's directory: soffice refuses to convert a file onto its own path
// (verified: same-path in-place conversion errors "SfxBaseModel::impl_store
// ... failed").
export async function runSoffice({ jobDir, spawn, extraArgs }) {
  const userInstallation = await prepareProfile(jobDir);
  return runCommand({
    spawn,
    cmd: SOFFICE_BIN,
    args: ['--headless', '--invisible', '--nologo', '--nofirststartwizard', userInstallation, ...extraArgs],
    cwd: jobDir,
  });
}
