import { readFileSync, writeFileSync, copyFileSync } from "node:fs";
import config from "../tome.config.js";

// Tome 0.9 emits relative canonical URLs and minimal detail-page heads.
// Complete static metadata for the custom domain and direct page visits.
const ids = config.navigation.flatMap((group) => group.pages);
const template = readFileSync("index.html", "utf8");
const sharedHead = template.match(/    <meta name="theme-color"[^>]*>/)[0]
  + "\n" + template.match(/    <link rel="icon"[^>]*>/)[0]
  + "\n" + template.match(/    <link rel="preconnect"[^>]*>/g).join("\n")
  + "\n" + template.match(/    <link href="https:\/\/fonts.googleapis.com[^>]*>/)[0];
for (const id of ids) {
  const path = id === "index" ? "out/index.html" : `out/${id}/index.html`;
  const url = `${config.baseUrl}/${id === "index" ? "" : id}`;
  let html = readFileSync(path, "utf8").replace(/<link rel="canonical"[^>]*>\s*/g, "");
  if (id !== "index") html = html.replace("</head>", `${sharedHead}\n</head>`);
  html = html.replace("</head>", `<link rel="canonical" href="${url}">\n</head>`);
  writeFileSync(path, html);
}
const urls = ids.map((id) => `<url><loc>${config.baseUrl}/${id === "index" ? "" : id}</loc></url>`).join("\n");
writeFileSync("out/sitemap.xml", `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n${urls}\n</urlset>\n`);
writeFileSync("out/.nojekyll", "");
// Let Tome's client router handle unknown paths on GitHub Pages.
copyFileSync("out/index.html", "out/404.html");
console.log(`Completed custom-domain metadata for ${ids.length} pages.`);
