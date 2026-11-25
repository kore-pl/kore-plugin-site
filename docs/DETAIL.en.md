# Nginx Site Management Plugin (site)

## What it does

kore's M2 site-management plugin handles the nginx site lifecycle: `site.create` atomically writes a separate `conf.d/kore-site-<domain>.conf`, validates with `nginx -t` (rollback and `E_INVALID` on failure), then runs `nginx -s reload` (failure returns `E_UPSTREAM`, explicitly meaning "written but not applied").
It never touches the main `nginx.conf`, never uses `default_server` and bundles no nginx/caddy/httpd binary; in host mode validation and reload go through the kernel privilege-escalation channel — the plugin itself is not root.

## Protocol and compatibility

Protocol `kore.plugin.v1`, `lifecycle=resident`, `depth_level=production`, `requires_kore >=0.1.1 <0.2.0`; the only third-party dependency is `github.com/pengtang1024/kore/pkg/pluginapi v0.1.1`.

## Main capabilities

- Create / delete / disable / enable / status / list / get; every write validates and reloads, rolling back on failure.
- L2: SSL bind / unbind (TLS1.2+, 80→443 redirect by default), rewrite templates (wordpress / laravel / thinkphp / discuz / nextjs), reverse proxy with WebSocket support, exact-path redirects and the index directive.
- PHP-FPM: `setPHP` / `unsetPHP` / `getPHP` detect and configure PHP for a site; unbinding is refused while a PHP rewrite template is active, avoiding a site-wide 404.
- `clone` copies a site's configuration; `setPassword` / `clearPassword` configure Basic auth (passwords are never echoed); `traffic` parses access logs read-only, returning `available=false` when logs are unreadable rather than inventing data.
- `config.get` / `config.set` read and write five runtime settings (atomic 0600 state file, effective immediately).
- Tri-state semantics: `test_skipped` / `reload_skipped` mean no nginx is available; in that case `test_passed=false` does not indicate a site failure.

## Install and verification

`make check` is the local gate; `make dist` produces a bit-for-bit reproducible `dist/site-0.8.2.tar.gz`; root actions and the permission model are documented in the README.
Report vulnerabilities privately per SECURITY.md; MIT license; contributions: CONTRIBUTING.md.