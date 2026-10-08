import assert from "node:assert/strict";
import { existsSync, readFileSync, readdirSync } from "node:fs";
import config from "../tome.config.js";

// Check deployable output, including direct visits to nested Pages routes.
for (const id of config.navigation.flatMap((group) => group.pages)) {
  const path = id === "index" ? "out/index.html" : `out/${id}/index.html`;
  const html = readFileSync(path, "utf8");
  assert(html.includes(`href="${config.baseUrl}/${id === "index" ? "" : id}"`), `Missing canonical URL: ${id}`);
  assert(html.includes("/favicon.svg"), `Missing favicon: ${id}`);
  assert(html.includes("fonts.googleapis.com"), `Missing fonts: ${id}`);
  for (const match of html.matchAll(/(?:src|href)="(\/assets\/[^"?#]+)"/g)) {
    assert(existsSync(`out${match[1]}`), `Missing asset: ${match[1]}`);
  }
}
for (const path of ["CNAME", ".nojekyll", "404.html", "logo.png", "favicon.svg", "search.json", "sitemap.xml", "llms.txt", "llms-full.txt"]) {
  assert(existsSync(`out/${path}`), `Missing build output: ${path}`);
}
assert.equal(readFileSync("out/CNAME", "utf8").trim(), "go-bonnie.dev");
const css = readdirSync("out/assets").filter((path) => path.endsWith(".css")).map((path) => readFileSync(`out/assets/${path}`, "utf8")).join("\n");
assert(css.includes("--bg:#080b20"), "Missing custom navy palette");
assert(css.includes("--ac:#00cbea"), "Missing custom cyan accent");
console.log("Checked generated routes, assets, metadata, palette, and Pages files.");
