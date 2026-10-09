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
const escapeAttribute = (value) => value.replaceAll("&", "&amp;").replaceAll('"', "&quot;").replaceAll("<", "&lt;").replaceAll(">", "&gt;");
const imageUrl = `${config.baseUrl}/og.png`;
for (const id of ids) {
  const path = id === "index" ? "out/index.html" : `out/${id}/index.html`;
  const url = `${config.baseUrl}/${id === "index" ? "" : id}`;
  let html = readFileSync(path, "utf8").replace(/<link rel="canonical"[^>]*>\s*/g, "");
  if (id !== "index") html = html.replace("</head>", `${sharedHead}\n</head>`);
  // Replace renderer metadata with one complete, page-specific social card.
  const source = readFileSync(`pages/${id}.md`, "utf8");
  const title = escapeAttribute(source.match(/^title: (.+)$/m)[1] + " | BONNIE");
  const description = escapeAttribute(source.match(/^description: (.+)$/m)[1]);
  html = html.replace(/<meta\s+[^>]*(?:property|name)=["'](?:og:|twitter:)[^>]*>\s*/g, "");
  const socialHead = [
    `<meta property="og:type" content="website">`,
    `<meta property="og:site_name" content="BONNIE">`,
    `<meta property="og:locale" content="en_US">`,
    `<meta property="og:title" content="${title}">`,
    `<meta property="og:description" content="${description}">`,
    `<meta property="og:url" content="${url}">`,
    `<meta property="og:image" content="${imageUrl}">`,
    `<meta property="og:image:secure_url" content="${imageUrl}">`,
    `<meta property="og:image:type" content="image/png">`,
    `<meta property="og:image:width" content="1200">`,
    `<meta property="og:image:height" content="630">`,
    `<meta property="og:image:alt" content="BONNIE logo. Durable agent runs for Go. Survive a crash. Wait days for a human.">`,
    `<meta name="twitter:card" content="summary_large_image">`,
    `<meta name="twitter:title" content="${title}">`,
    `<meta name="twitter:description" content="${description}">`,
    `<meta name="twitter:image" content="${imageUrl}">`,
    `<meta name="twitter:image:alt" content="BONNIE logo. Durable agent runs for Go. Survive a crash. Wait days for a human.">`,
  ].join("\n");
  html = html.replace("</head>", `<link rel="canonical" href="${url}">\n${socialHead}\n</head>`);
  writeFileSync(path, html);
}
const urls = ids.map((id) => `<url><loc>${config.baseUrl}/${id === "index" ? "" : id}</loc></url>`).join("\n");
writeFileSync("out/sitemap.xml", `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n${urls}\n</urlset>\n`);
writeFileSync("out/.nojekyll", "");
// Let Tome's client router handle unknown paths on GitHub Pages.
copyFileSync("out/index.html", "out/404.html");
console.log(`Completed custom-domain metadata for ${ids.length} pages.`);
