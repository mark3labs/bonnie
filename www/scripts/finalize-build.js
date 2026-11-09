import { readFileSync, writeFileSync } from "node:fs";
import { processMarkdown } from "@tomehq/core";
import config from "../tome.config.js";

// Tome 0.9 ships each page as an empty <div id="tome-root"> plus the raw
// markdown in a display:none div for Pagefind. A scanner that does not run
// JavaScript sees either a blank page or hidden text that differs from what
// a reader sees. Google Safe Browsing classified that cloaking-like shape as
// "deceptive" again after each manual review (issue #61). Serve the rendered
// page visibly in #tome-root instead. React's createRoot replaces it on load,
// so readers see the same text. The Pagefind index is built before this step.
const escapeHtml = (value) => value.replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;");
const staticNav = `<nav aria-label="Documentation">\n${config.navigation.map((group) =>
  `<h2>${escapeHtml(group.group)}</h2>\n<ul>${group.pages.map((id) => {
    const title = readFileSync(`pages/${id}.md`, "utf8").match(/^title: (.+)$/m)[1];
    return `<li><a href="/${id === "index" ? "" : id}">${escapeHtml(title)}</a></li>`;
  }).join("")}</ul>`).join("\n")}\n</nav>`;
const staticRoot = (body) => `<div id="tome-root"><div class="bonnie-static">\n<main>\n${body}\n</main>\n${staticNav}\n</div></div>`;
const hiddenText = /<div data-pagefind-body style="display:none">[\s\S]*?<\/div>\s*(?=<\/body>)/;
// Tome's JSON-LD names a /search?q= URL that does not exist on static hosting.
const searchAction = /,"potentialAction":\{"@type":"SearchAction"[^}]*\}/;
const withStaticBody = (html, body) => {
  html = html.replace(hiddenText, "").replace(searchAction, "");
  if (!html.includes('<div id="tome-root"></div>')) throw new Error("finalize-build: missing empty #tome-root");
  return html.replace('<div id="tome-root"></div>', staticRoot(body));
};

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
  const page = await processMarkdown(source, `pages/${id}.md`);
  html = withStaticBody(html, `<h1>${escapeHtml(page.frontmatter.title)}</h1>\n${page.html}`);
  writeFileSync(path, html);
}
const urls = ids.map((id) => `<url><loc>${config.baseUrl}/${id === "index" ? "" : id}</loc></url>`).join("\n");
writeFileSync("out/sitemap.xml", `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n${urls}\n</urlset>\n`);
writeFileSync("out/.nojekyll", "");
// Let Tome's client router handle unknown paths on GitHub Pages. The static
// body says "not found" instead of repeating the home page under every URL.
const notFound = readFileSync("out/index.html", "utf8")
  .replace(/<link rel="canonical"[^>]*>\s*/g, "")
  .replace(/<div id="tome-root">[\s\S]*<\/div><\/div>/, '<div id="tome-root"></div>');
writeFileSync("out/404.html", withStaticBody(notFound, `<h1>Page not found</h1>\n<p>This page does not exist. Use the links below, or go to the <a href="/">BONNIE documentation home</a>.</p>`));
console.log(`Completed custom-domain metadata for ${ids.length} pages.`);
