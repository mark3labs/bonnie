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
  const source = readFileSync(`pages/${id}.md`, "utf8");
  const escapeAttribute = (value) => value.replaceAll("&", "&amp;").replaceAll('"', "&quot;").replaceAll("<", "&lt;").replaceAll(">", "&gt;");
  const title = escapeAttribute(source.match(/^title: (.+)$/m)[1] + " | BONNIE");
  const description = escapeAttribute(source.match(/^description: (.+)$/m)[1]);
  const expected = {
    "og:type": "website",
    "og:site_name": "BONNIE",
    "og:title": title,
    "og:description": description,
    "og:url": `${config.baseUrl}/${id === "index" ? "" : id}`,
    "og:image": `${config.baseUrl}/og.png`,
    "og:image:secure_url": `${config.baseUrl}/og.png`,
    "og:image:type": "image/png",
    "og:image:width": "1200",
    "og:image:height": "630",
    "twitter:card": "summary_large_image",
    "twitter:title": title,
    "twitter:description": description,
    "twitter:image": `${config.baseUrl}/og.png`,
  };
  for (const [key, value] of Object.entries(expected)) {
    const attribute = key.startsWith("og:") ? "property" : "name";
    assert.equal(html.split(`${attribute}="${key}"`).length - 1, 1, `Duplicate or missing ${key}: ${id}`);
    assert(html.includes(`<meta ${attribute}="${key}" content="${value}">`), `Incorrect ${key}: ${id}`);
  }
  assert(html.includes('property="og:image:alt" content="BONNIE logo.'), `Missing OG image alt: ${id}`);
  assert(html.includes('name="twitter:image:alt" content="BONNIE logo.'), `Missing Twitter image alt: ${id}`);
  for (const match of html.matchAll(/(?:src|href)="(\/assets\/[^"?#]+)"/g)) {
    assert(existsSync(`out${match[1]}`), `Missing asset: ${match[1]}`);
  }
}
for (const path of ["CNAME", ".nojekyll", "404.html", "logo.png", "quick-start.gif", "favicon.svg", "search.json", "sitemap.xml", "llms.txt", "llms-full.txt"]) {
  assert(existsSync(`out/${path}`), `Missing build output: ${path}`);
}
const card = readFileSync("out/og.png");
assert.equal(card.subarray(0, 8).toString("hex"), "89504e470d0a1a0a", "Invalid OG PNG");
assert.equal(card.readUInt32BE(16), 1200, "Incorrect OG image width");
assert.equal(card.readUInt32BE(20), 630, "Incorrect OG image height");
assert(card.equals(readFileSync("public/og.png")), "OG image changed during build");
assert.equal(readFileSync("out/quick-start.gif").subarray(0, 6).toString(), "GIF89a", "Invalid demo GIF");
assert(readFileSync("out/assets/" + readdirSync("out/assets").find((path) => path.startsWith("page-") && path.endsWith(".js")), "utf8").includes("/quick-start.gif"), "Home page must include the demo");
assert.equal(readFileSync("out/CNAME", "utf8").trim(), "go-bonnie.dev");
const css = readdirSync("out/assets").filter((path) => path.endsWith(".css")).map((path) => readFileSync(`out/assets/${path}`, "utf8")).join("\n");
assert(css.includes("--bg:#080b20"), "Missing custom navy palette");
assert(css.includes("--ac:#00cbea"), "Missing custom cyan accent");
console.log("Checked generated routes, assets, metadata, palette, and Pages files.");
