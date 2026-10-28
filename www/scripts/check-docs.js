import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";
import config from "../tome.config.js";

// Check authored pages, navigation, and internal links before Tome builds.
function files(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name);
    return entry.isDirectory() ? files(path) : [path];
  });
}
// This guard covers documentation examples, not general shell syntax.
function hasRemoteShellPipeline(text) {
  const normalized = text.replace(/\\\r?\n\s*(?:#\s*)?/g, " ");
  return /\b(?:curl|wget)\b[^\n|]*\|\s*(?:ba)?sh\b/.test(normalized);
}

for (const text of [
  "curl -fsSL https://example.com/install.sh | bash",
  "curl -fsSL https://example.com/install.sh \\\n  | bash -s -- --version v0.35.0",
  "wget -qO- https://example.com/install.sh | sh -s --",
  "wget -qO- https://example.com/install.sh \\\n  | bash",
]) assert(hasRemoteShellPipeline(text), `Missed remote shell pipeline: ${text}`);
for (const text of [
  "curl -fsSLO https://example.com/install.sh\nless install.sh\nbash install.sh",
  "wget https://example.com/install.sh\nbash install.sh",
  "curl -s localhost:8080/bonnie/v1/runs -d '{}'",
]) assert(!hasRemoteShellPipeline(text), `False remote shell pipeline: ${text}`);

const pages = files("pages").filter((path) => path.endsWith(".md"));
for (const path of [...pages, "../README.md", "../install.sh"]) {
  const text = readFileSync(path, "utf8");
  // Limit the installer scan to its comment preamble, not runtime code.
  const examples = path === "../install.sh" ? text.split(/^set -/m)[0] : text;
  assert(!hasRemoteShellPipeline(examples), `Remote download piped to shell: ${path}`);
}
const ids = new Set(pages.map((path) => relative("pages", path).replace(/\.md$/, "")));
const navigation = config.navigation.flatMap((group) => group.pages);
assert.equal(new Set(navigation).size, navigation.length, "Duplicate navigation page");
for (const id of navigation) assert(ids.has(id), `Missing navigation page: ${id}`);
for (const path of pages) {
  const text = readFileSync(path, "utf8");
  assert.match(text, /^---\ntitle: .+\ndescription: .+\n(?:toc: false\n)?---\n/, `Invalid frontmatter: ${path}`);
  assert.equal((text.match(/^```/gm) ?? []).length % 2, 0, `Unclosed code fence: ${path}`);
  assert(!/\]\((?!https?:)[^\s)]*\.md(?:#[^)]*)?\)/.test(text), `Use site routes, not Markdown file links: ${path}`);
  const id = relative("pages", path).replace(/\.md$/, "");
  assert(navigation.includes(id), `Page missing from navigation: ${id}`);
  for (const match of text.matchAll(/\]\(\/([^\s)#]*)(?:#[^\s)]*)?\)/g)) {
    assert(ids.has(match[1] || "index"), `Broken page link in ${path}: ${match[0]}`);
  }
}
assert.equal(config.baseUrl, "https://go-bonnie.dev");
assert.equal(readFileSync("public/CNAME", "utf8").trim(), "go-bonnie.dev");
console.log(`Checked ${pages.length} pages, navigation, and internal page links.`);
