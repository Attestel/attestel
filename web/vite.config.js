import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// Aux services the browser calls directly. Each is exposed under the SAME "/svc/<name>" prefix in
// dev and in prod, so a request is never cross-origin and CORS never enters the picture:
//   dev  — the proxy below strips the prefix and forwards to the local port.
//   prod — deploy/nginx.conf.template does the identical strip via `proxy_pass http://127.0.0.1:<port>/`.
// The session cookie (auth) only works because of this: a credentialed cross-origin request to a
// different host is blocked by CORS, and its cookie would be a third-party cookie browsers now drop.
//
// THE PORTS ARE OVERRIDABLE, AND THE DEFAULTS ARE EXACTLY WHAT THEY WERE. Wave 4 had two lanes hit
// the same wall: `:8080` was occupied by an unrelated user process, so the gateway had to run
// elsewhere, and this file hardcoded every target. Lane 4B worked around it with `VITE_GATEWAY_URL`
// and correctly did not edit this file; the integrator's call is that a dev-server port that cannot
// be moved is friction with no upside. Set `VITE_GATEWAY_URL` or `VITE_PORT_<NAME>` to move one;
// change nothing and every target is the port it has always been.
const port = (name, fallback) => Number(process.env[`VITE_PORT_${name.toUpperCase()}`]) || fallback;

const GATEWAY = process.env.VITE_GATEWAY_URL || `http://localhost:${port("gateway", 8080)}`;

const SVC = {
  auth: port("auth", 8099),
  alerts: port("alerts", 8095),
  journal: port("journal", 8096),
  paper: port("paper", 8097),
  feedback: port("feedback", 8098),
};

const svcProxy = Object.fromEntries(
  Object.entries(SVC).map(([name, port]) => [
    `/svc/${name}`,
    {
      target: `http://localhost:${port}`,
      changeOrigin: true,
      rewrite: (p) => p.replace(new RegExp(`^/svc/${name}`), ""),
    },
  ])
);

// appSlashRedirect mirrors nginx's `location = /app { return 301 /app/; }` (deploy/nginx.conf.template)
// in the dev server. The auth service builds its post-login redirect as TrimRight(APP_URL,"/") + "#view"
// (auth/google.go appRedirect), so the browser always arrives at /app#view — no trailing slash. In
// production nginx redirects that to /app/; without this, the dev server answers with its "public base
// URL is /app/" notice instead of the app, so Google sign-in appears to fail on the very last hop.
// The fragment is browser-side and survives the redirect on its own; the query string is carried here.
const appSlashRedirect = () => ({
  name: "app-slash-redirect",
  configureServer(server) {
    server.middlewares.use((req, res, next) => {
      const url = req.url || "";
      if (url === "/app" || url.startsWith("/app?")) {
        res.writeHead(301, { Location: "/app/" + url.slice("/app".length) });
        res.end();
        return;
      }
      next();
    });
  },
});

export default defineConfig({
  // The React product lives under /app/ in production; the Attestel landing page owns "/".
  // Baking the base in means Vite emits /app/assets/... URLs, so a hard refresh on any product
  // route resolves its chunks. Dev serves at http://localhost:5173/app/ for the same reason —
  // one URL shape everywhere. Absolute API paths (/api, /health, /svc/*) are unaffected.
  base: "/app/",
  plugins: [react(), tailwindcss(), appSlashRedirect()],
  server: {
    port: 5173,
    proxy: {
      // dev convenience: /api + /health -> Go gateway, avoids CORS in local dev.
      // /health backs the gateway HealthDot (only /api was proxied before).
      "/api": GATEWAY,
      "/health": GATEWAY,
      ...svcProxy,
    },
  },
  build: {
    // Split the heavy vendor libs so the main bundle stays small and caches well.
    rollupOptions: {
      output: {
        // Function form, not the object form. Vite 8 replaces rollup with rolldown, which dropped
        // object `manualChunks` entirely — it fails the build with "Expected Function but received
        // Object" — and the function form is what both bundlers accept. Written this way, the Vite
        // major becomes an ordinary dependency bump instead of a bump plus a config migration.
        // Verified to produce the same three chunks under vite 6.4.3 and vite 8.2.2.
        manualChunks(id) {
          if (/node_modules\/(react|react-dom|scheduler)\//.test(id)) return "react";
          if (id.includes("lightweight-charts")) return "charts";
          if (id.includes("framer-motion")) return "motion";
        },
      },
    },
  },
});
