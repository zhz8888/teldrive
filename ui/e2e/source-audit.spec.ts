import { expect, test } from "@playwright/test";
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, relative } from "node:path";

const sourceRoot = join(process.cwd(), "src");

function sourceFiles(directory: string): string[] {
  return readdirSync(directory).flatMap((entry) => {
    const path = join(directory, entry);
    return statSync(path).isDirectory()
      ? sourceFiles(path)
      : /\.(ts|tsx|css)$/.test(path)
        ? [path]
        : [];
  });
}

test("all active API consumers import generated OpenAPI types", () => {
  const apiConsumers = sourceFiles(sourceRoot).filter((path) => {
    if (path.endsWith("api/schema.ts") || path.endsWith("gen/api.d.ts")) return false;
    if (!path.endsWith(".ts") && !path.endsWith(".tsx")) return false;
    const source = readFileSync(path, "utf8");
    return (
      source.includes("$api.") || source.includes("components[") || source.includes("operations[")
    );
  });
  expect(apiConsumers.length).toBeGreaterThan(10);
  for (const path of apiConsumers) {
    const source = readFileSync(path, "utf8");
    const importsGeneratedTypes =
      /from\s+["'](?:@\/api(?:\/client|\/schema)?|[^"']*\/api(?:\/client|\/schema)?|\.\/schema)["']/.test(
        source,
      );
    expect(importsGeneratedTypes, relative(sourceRoot, path)).toBe(true);
  }
});

test("the login form keeps its per-step autocomplete semantics", () => {
  const source = readFileSync(join(sourceRoot, "routes/login.tsx"), "utf8");
  // A bare text field followed by a password field reads as a credential pair to
  // browsers, which then replay the typed text into the field that replaced it:
  // the Telegram code used to reappear in the two-step password input. These
  // hints are what keep the two apart.
  expect(source).toContain('autoComplete="tel"');
  expect(source).toContain('autoComplete="one-time-code"');
  expect(source).toContain('autoComplete="current-password"');
});

test("the document loads every script from a file, never inline", () => {
  // The server answers the UI with `script-src 'self'`, which refuses inline
  // scripts outright, so the pre-paint theme script has to stay a separate file
  // that index.html references by src. An inline script would simply never run
  // in a production deployment, and the interface would flash the wrong theme.
  const html = readFileSync(join(process.cwd(), "index.html"), "utf8");
  const inlineScripts = [...html.matchAll(/<script(?![^>]*\ssrc=)[^>]*>([\s\S]*?)<\/script>/g)]
    .map((match) => match[1].trim())
    .filter((body) => body !== "");
  expect(inlineScripts).toEqual([]);
  expect(html).toContain('src="/theme.js"');
});

// Han, Hangul and kana ranges plus the compatibility ideographs: the characters a
// translated string is made of. Latin accents are deliberately not included.
const cjkPattern = /[\u3000-\u303f\u3040-\u30ff\u3400-\u4dbf\u4e00-\u9fff\uf900-\ufaff\uff00-\uffef]/;
const translationRoot = join(sourceRoot, "lib", "i18n");

test("translated text lives only in the translation module", () => {
  // Interface copy is keyed, never inlined: a literal Chinese string outside the
  // catalogs is either a missed translation or a message that belongs in a log.
  const offenders: string[] = [];
  for (const path of sourceFiles(sourceRoot)) {
    if (path.startsWith(translationRoot)) continue;
    const lines = readFileSync(path, "utf8").split("\n");
    lines.forEach((line, index) => {
      if (cjkPattern.test(line)) offenders.push(`${relative(sourceRoot, path)}:${index + 1}`);
    });
  }
  expect(offenders).toEqual([]);
});

test("log output is never translated", () => {
  // Console output, job traces and diagnostics are read by operators and pasted
  // into bug reports, so they stay English whatever the interface language is.
  const offenders: string[] = [];
  for (const path of sourceFiles(sourceRoot)) {
    const lines = readFileSync(path, "utf8").split("\n");
    lines.forEach((line, index) => {
      if (!/console\.(log|info|warn|error|debug)\s*\(/.test(line)) return;
      if (/\bt\(|translate\(|useI18n\(/.test(line)) {
        offenders.push(`${relative(sourceRoot, path)}:${index + 1}`);
      }
    });
  }
  expect(offenders).toEqual([]);
});
