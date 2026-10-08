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
const pages = files("pages").filter((path) => path.endsWith(".md"));
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
