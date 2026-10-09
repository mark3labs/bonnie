# BONNIE documentation site

The site uses Tome 0.9.0, the latest published CLI and theme version when this
site was created. The production URL is https://go-bonnie.dev.

## Local development

Use Node 24 and Bun 1.3.13, as the Pages workflow does.

```bash
cd www
bun install --frozen-lockfile
bun run dev
```

Edit Markdown under `pages/`. Add each new page to `tome.config.js` navigation.
Use `title` and `description` frontmatter. Use root-relative page links without
`.md`, for example `/guides/sandboxes`. Check source comments and tests before
changing API claims. These docs describe the checkout; an older release can
have a different API. Release history is linked from the site.

The artwork is copied from `../logo.png` to `public/logo.png`. Update both copies
when the source logo changes. The cyan, pink, and navy palette is in
`styles/custom.css`. Tome sets inline palette variables, so the overrides target
its layout class. `index.html` loads the stylesheet and fonts.

## Validate and build

```bash
bun run test
```

The test checks frontmatter, navigation, and internal page links; builds the
site; then checks generated routes, assets, custom-domain metadata, and Pages
files. Generated files live in `out/` and are not committed.

Tome 0.9 emits relative canonical links and minimal HTML heads on detail pages.
`scripts/finalize-build.js` adds absolute canonical links, shared font and
favicon metadata, a sitemap, `.nojekyll`, and the Pages 404 fallback. No Tome
package code is changed. Tome also generates search and LLM-readable exports.
Each static page also receives Open Graph and Twitter large-image card tags.
Titles and descriptions come from page frontmatter; URLs use the HTTPS custom
domain. All pages share `public/og.png`, a committed 1200 × 630 PNG with the
BONNIE logo and site colors. Build checks verify the card dimensions, copied
image, and page-specific tags. This does not depend on Tome's optional OG renderer.

To update the card after a logo or design change, run from `www/`:

```bash
go run ./scripts/generate-og
bun run test
```

The generator uses the repository's existing Go image dependency and embedded
Go fonts. Commit the generator and `public/og.png`, not `out/`. After deployment,
check a nested page's HTML and request a fresh preview from the social platform;
platforms can cache old images and metadata.

## GitHub Pages deployment

Kit's `../kit/.github/workflows/pages.yml` builds with Bun and pushes `www/out`
to a `gh-pages` branch. BONNIE uses GitHub's Pages artifact deployment instead.
There is no `gh-pages` branch to manage.

1. In the BONNIE repository, open **Settings → Pages**.
2. Select **GitHub Actions** as the build and deployment source.
3. Merge the site and `.github/workflows/pages.yml` into `master`.
4. Run **Build and deploy documentation** from Actions, or let the push run it.
5. Confirm that the `github-pages` environment permits deployment from `master`.

Pull requests build and validate the site without deploying it. Deployments
need only the built-in `GITHUB_TOKEN`; no personal token or DNS credential is
needed in the workflow.

## Connect go-bonnie.dev

Configure the domain on the repository that publishes this site, not on Kit.

1. Recommended: verify ownership under the GitHub account or organization
   **Settings → Pages → Add a domain**. GitHub gives you a TXT record named
   `_github-pages-challenge-<owner>`. Add its exact value through your DNS
   provider and keep the record after verification.
2. In the BONNIE repository's **Settings → Pages**, set the custom domain to
   `go-bonnie.dev` and save. `public/CNAME` already contains that domain, and
   `tome.config.js` uses its HTTPS URL. The repository setting is still needed
   for artifact-based deployment.
3. At your DNS provider, configure these apex records. Replace conflicting
   apex A/AAAA records, but retain unrelated mail and verification records.

   | Type | Name | Value |
   | --- | --- | --- |
   | A | `@` | `185.199.108.153` |
   | A | `@` | `185.199.109.153` |
   | A | `@` | `185.199.110.153` |
   | A | `@` | `185.199.111.153` |

   Optional IPv6 records:

   | Type | Name | Value |
   | --- | --- | --- |
   | AAAA | `@` | `2606:50c0:8000::153` |
   | AAAA | `@` | `2606:50c0:8001::153` |
   | AAAA | `@` | `2606:50c0:8002::153` |
   | AAAA | `@` | `2606:50c0:8003::153` |

   Alternatively, use your provider's ALIAS/ANAME support to point the apex to
   `mark3labs.github.io`, if it supports GitHub Pages correctly.
4. Optional: add a `www` CNAME with value `mark3labs.github.io` (no path).
   GitHub Pages redirects between the apex and `www` when DNS is configured.
   Do not use a wildcard record. If you use Cloudflare, start with DNS-only
   records until GitHub completes its DNS check and certificate setup.
5. Wait for the Pages DNS check and TLS certificate to finish. DNS propagation
   can take up to 24 hours. Enable **Enforce HTTPS** when it becomes available.
   `.dev` domains require HTTPS in browsers.
6. Check `https://go-bonnie.dev`, a direct nested URL such as
   `https://go-bonnie.dev/channels/http`, the logo, navigation, and search.

If a CAA policy restricts certificate issuers, permit `letsencrypt.org` for
GitHub Pages. See the current [GitHub custom-domain instructions](https://docs.github.com/en/pages/configuring-a-custom-domain-for-your-github-pages-site/managing-a-custom-domain-for-your-github-pages-site)
before changing DNS. No DNS or repository settings are changed by a local build.
