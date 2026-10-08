// Build step 1's proof: a real Chromium loads a Streamlit app through booth-core's iframe proxy,
// inside an iframe on core's own origin (standing in for the shell, which is same-origin with core
// by construction, ADR 0069), and the app's websocket session works both ways.
//
//   node iframe-path.mjs <core base URL> <viewer iframe URL> <viewer sub> <outsider iframe URL>
//
// The iframe URLs are core's own (GET /api/modules/streamlit/iframe-url), minted in-cluster from
// real Keycloak tokens a moment earlier; core's navigation token is valid for one minute.
import { chromium } from "playwright";

const [base, viewerURL, viewerSub, outsiderURL] = process.argv.slice(2);
if (!base || !viewerURL || !viewerSub || !outsiderURL) {
  console.error("usage: node iframe-path.mjs <core base URL> <viewer iframe URL> <viewer sub> <outsider iframe URL>");
  process.exit(2);
}

// Core issues /iframe/streamlit/?<token>; its entry handler accepts any path under the module, so
// point the same token at the app.
const toApp = (u) => u.replace(/^\/iframe\/streamlit\/\?/, "/iframe/streamlit/apps/demo/?");

const failures = [];
const check = (ok, what) => {
  console.log(`${ok ? "ok  " : "FAIL"} ${what}`);
  if (!ok) failures.push(what);
};

const browser = await chromium.launch();
try {
  // --- A workspace member opens the app inside an iframe on the shell's origin.
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  const sockets = [];
  page.on("websocket", (ws) => sockets.push(ws.url()));
  await page.route(`${base}/__booth_parent`, (route) =>
    route.fulfill({
      contentType: "text/html",
      body: `<!doctype html><title>shell stand-in</title>
<iframe id="app" src="${toApp(viewerURL)}" style="width:1200px;height:900px"
  sandbox="allow-scripts allow-same-origin allow-forms allow-popups allow-downloads"></iframe>`,
    }),
  );
  await page.goto(`${base}/__booth_parent`);
  const app = page.frameLocator("#app");

  // Streamlit renders script output only over its websocket session, so seeing it proves the
  // session was established through core and the module's proxy.
  const marker = (m) => app.getByText(m, { exact: true });
  await marker(`marker:user=${viewerSub}`).waitFor({ timeout: 120_000 });
  check(true, `the app rendered over its websocket and saw the viewer as ${viewerSub}`);
  check(await marker("marker:workspace=acme-analytics").isVisible(), "workspace is the app's workspace");
  check(await marker("marker:role=viewer").isVisible(), "role is the viewer's real role");
  check(await marker("marker:identity-header-visible=False").isVisible(), "X-Booth-Identity never reached user code");
  check(await marker("marker:core-cookie-visible=False").isVisible(), "core's booth_iframe_session cookie never reached user code");

  // Browser → app: a widget event goes up the websocket and the rerun's output comes back.
  await app.getByRole("button", { name: "Rerun" }).click();
  await marker("marker:button-roundtrip=ok").waitFor({ timeout: 30_000 });
  check(true, "a button click round-tripped over the websocket");

  console.log(`websockets opened: ${JSON.stringify(sockets)}`);
  check(sockets.some((u) => u.endsWith("/_stcore/stream")), "Streamlit's /_stcore/stream websocket was opened");

  // Recorded, not asserted: whether the app's document can script the embedding page. The shell's
  // iframe is same-origin with it (ADR 0069) and sandboxed with allow-same-origin, so it can,
  // which matters here because the app's code is written by someone other than the viewer.
  const frame = page.frames().find((f) => f.url().includes("/iframe/streamlit/apps/demo/"));
  const parentReachable = await frame.evaluate(() => {
    try {
      return typeof window.parent.document.title === "string";
    } catch {
      return false;
    }
  });
  console.log(`info: the app's document can script the embedding page: ${parentReachable}`);

  // --- The module's own page, through core, as the same viewer (core's session cookie is live):
  // it loads, calls its API with relative URLs, shows the ADR 0105 trust note, and offers a viewer
  // no authoring controls.
  const ui = await ctx.newPage();
  await ui.route(`${base}/__booth_parent_ui`, (route) =>
    route.fulfill({
      contentType: "text/html",
      body: `<!doctype html><iframe id="ui" src="/iframe/streamlit/" style="width:1200px;height:900px"
  sandbox="allow-scripts allow-same-origin allow-forms allow-popups allow-downloads"></iframe>`,
    }),
  );
  await ui.goto(`${base}/__booth_parent_ui`);
  const page2 = ui.frameLocator("#ui");
  await page2.getByRole("heading", { name: "Streamlit apps" }).waitFor({ timeout: 60_000 });
  check(
    (await page2.getByRole("note").textContent())?.includes("App code runs in your browser with access to your Booth session") ?? false,
    "the module UI shows the trust note (ADR 0105)",
  );
  await page2.getByText("Only workspace owners can create or change apps").waitFor({ timeout: 30_000 });
  check((await page2.getByRole("button", { name: "New app" }).count()) === 0, "a viewer is offered no 'New app'");
  await ctx.close();

  // --- A member of another workspace is refused by the module (ADR 0104 item 5).
  const octx = await browser.newContext();
  const opage = await octx.newPage();
  const resp = await opage.goto(`${base}${toApp(outsiderURL)}`);
  check(resp.status() === 404, `a member of another workspace gets 404 (got ${resp.status()})`);
  await octx.close();
} finally {
  await browser.close();
}

if (failures.length) {
  console.error(`${failures.length} check(s) failed`);
  process.exit(1);
}
console.log("iframe path: all checks passed");
