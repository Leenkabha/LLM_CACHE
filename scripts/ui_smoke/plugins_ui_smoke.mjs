// Optional DOM-level smoke test of the Plugins page (internal/orchestrator/web/plugins.html).
// It loads the real page in jsdom, replaces fetch() with a fake admin API, and checks that
// rendering, the add-plugin flow, confirmation dialogs and secret handling behave.
//
//   cd scripts/ui_smoke && npm install && node plugins_ui_smoke.mjs
import { JSDOM } from 'jsdom';
import { readFileSync } from 'node:fs';
import assert from 'node:assert/strict';

const html = readFileSync(new URL('../../internal/orchestrator/web/plugins.html', import.meta.url), 'utf8');
const SECRET = 'S3CRET-typed-by-the-admin';

const plugin = (o) => ({
  id: 'plg_1', name: 'my-llm', version: '1.0.0', type: 'llm', slot: 'llm', mode: 'endpoint', state: 'verified',
  health: { status: 'healthy' }, active: false, secrets: [{ name: 'ENDPOINT_TOKEN', configured: true }],
  source: { endpoint_url: 'https://plugin.example.com' }, updated_at: new Date().toISOString(), ...o,
});
const comps = ['llm', 'embedder', 'vector-store', 'persistence', 'queue', 'policy', 'embedding-model', 'vector-index', 'similarity-metric']
  .map((t) => ({ type: t, slot: t, python: /model|index|metric/.test(t), builtin: 'built-in-' + t }));

const calls = [];
const HOSTILE = '<img src=x onerror="window.pwned=1"><script>window.pwned=2</script>';
let plugins = [plugin({ id: 'plg_a', state: 'building', name: 'busy-one' }), plugin({ id: 'plg_b', state: 'active', active: true, name: 'live-one' }),
  plugin({ id: 'plg_h', name: HOSTILE, description: HOSTILE, state_detail: HOSTILE, source: { endpoint_url: HOSTILE }, health: { status: 'unhealthy', message: HOSTILE } })];
let activateBody = null;

function fakeFetch(url, opts = {}) {
  calls.push({ url, opts });
  const json = (code, body) => Promise.resolve({ ok: code < 400, status: code, json: async () => body });
  assert.equal(opts.headers.Authorization, 'Bearer tok-123', 'every call carries the bearer header');
  assert.equal(opts.credentials, 'omit');
  assert.ok(!String(url).includes('tok-123') && !String(url).includes(SECRET), 'no credential in a URL');
  if (url === '/admin/plugins' && opts.method === 'GET') return json(200, { plugins, components: comps, settings: { controller_configured: false, allow_insecure_endpoints: true }, active: {} });
  if (url.startsWith('/admin/audit')) return json(200, { events: [{ time: new Date().toISOString(), action: 'install', outcome: 'ok', type: 'llm', detail: 'x' }] });
  if (url === '/admin/plugins/verify') return json(200, { valid: true, name: 'my-llm', version: '1.0.0', type: 'llm', config_schema: { model: { type: 'string', required: true }, level: { type: 'enum', enum: ['a', 'b'], default: 'a' } }, verification: { passed: true, summary: 'llm contract: 8 passed', checks: [{ name: 'reply', status: 'pass' }] } });
  if (url === '/admin/plugins/install') { calls.at(-1).body = JSON.parse(opts.body); return json(202, { plugin: plugin({ id: 'plg_new', state: 'draft' }) }); }
  if (url === '/admin/plugins/plg_new') return json(200, { plugin: plugin({ id: 'plg_new', state: 'verified' }) });
  if (url === '/admin/plugins/plg_new/activate') {
    activateBody = JSON.parse(opts.body);
    if (!activateBody.confirmations.embedder_compat) return json(409, { error: 'model changed', requires: { field: 'embedder_compat', options: ['flush', 'reembed'], message: 'model changed', warning: 'flush deletes the cache' } });
    return json(200, { plugin: plugin({ id: 'plg_new', state: 'active', active: true }), warnings: [] });
  }
  return json(404, { error: 'not found' });
}

const dom = new JSDOM(html, { runScripts: 'dangerously', pretendToBeVisual: true, url: 'http://localhost:8080/plugins',
  beforeParse(w) {
    w.fetch = fakeFetch;
    // jsdom lacks <dialog>.showModal; emulate the parts the page uses.
    w.HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
    w.HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); this.dispatchEvent(new w.Event('close')); };
    w.confirm = () => true;
  } });
const { document } = dom.window;
const $ = (id) => document.getElementById(id);
const tick = (ms = 20) => new Promise((r) => setTimeout(r, ms));
const click = (el) => el.dispatchEvent(new dom.window.MouseEvent('click', { bubbles: true }));
const text = (el) => el.textContent.replace(/\s+/g, ' ');

// 1. sign-in
assert.equal($('app').hidden, true);
$('token').value = 'tok-123';
$('login-form').dispatchEvent(new dom.window.Event('submit', { cancelable: true, bubbles: true }));
await tick(50);
assert.equal($('app').hidden, false, 'signed in');
assert.equal($('token').value, '', 'the token input is cleared after sign-in');

// 2. components: all nine types, active one marked
assert.equal($('components').children.length, 9);
assert.match(text($('components')), /built-in-embedder/);
// 3. plugin cards show the real backend state labels
assert.match(text($('plugins')), /Building/);
assert.match(text($('plugins')), /Active/);
assert.match(text($('banner')), /Development mode/);
assert.match(text($('banner')), /controller/i);
// building plugin has no Delete; active plugin has Deactivate but no Delete
const btnTexts = (root) => [...root.querySelectorAll('button')].map((b) => b.textContent);
const cards = [...$('plugins').querySelectorAll('article')];
assert.ok(!btnTexts(cards[0]).includes('Delete'), 'no delete while building');
assert.ok(btnTexts(cards[1]).includes('Deactivate') && btnTexts(cards[1]).includes('Roll back') && !btnTexts(cards[1]).includes('Delete'));

// 4. hostile API data (in a name, description, detail, URL and health message) is rendered as
// text, never as markup: no element is created from it and no script runs.
assert.equal(document.querySelector('#plugins img'), null, 'no <img> was created from API data');
assert.equal(document.querySelectorAll('#plugins script').length, 0, 'no <script> was created from API data');
assert.ok(text($('plugins')).includes('<img src=x'), 'the hostile string is displayed literally');
assert.equal(dom.window.pwned, undefined, 'no injected script ran');
plugins = [];
await tick();

// 5. add plugin: verify -> config form -> install with secrets in the JSON body only
plugins = [];
click($('add-btn'));
assert.ok($('add-dlg').hasAttribute('open'));
assert.equal($('f-type').options.length, 9);
$('f-endpoint').value = 'https://plugin.example.com';
$('f-token').value = SECRET;
click($('verify-btn'));
await tick(60);
assert.match(text($('verify-out')), /my-llm 1\.0\.0 \(llm\)/);
assert.match(text($('verify-out')), /llm contract: 8 passed/);
const cfg = $('config-out');
assert.ok(cfg.querySelector('[data-cfg=model]'), 'a form field is generated from the manifest schema');
assert.equal(cfg.querySelector('[data-cfg=level]').tagName, 'SELECT');
assert.equal($('install-btn').hidden, false);
cfg.querySelector('[data-cfg=model]').value = 'demo';
click($('install-btn'));
await tick(80);
const inst = calls.find((c) => c.url === '/admin/plugins/install');
assert.ok(inst, 'install was called');
assert.equal(inst.body.secrets.ENDPOINT_TOKEN, SECRET, 'the secret travels in the request body');
assert.equal(inst.body.config.model, 'demo');
assert.ok(!calls.some((c) => String(c.url).includes(SECRET)), 'the secret is never in a URL');
assert.equal($('f-token').value, '', 'the secret input is cleared after submit');
assert.ok(!document.documentElement.outerHTML.includes(SECRET), 'the secret is not left anywhere in the DOM');

// 6. activation that needs a confirmation opens the dialog and retries with the choice
await tick(1600); // the page polls the new plugin until verified, then activates
assert.ok($('confirm-dlg').hasAttribute('open'), 'the compatibility dialog is shown');
assert.match(text($('confirm-body')), /flush deletes the cache/);
click(document.querySelector('input[name=c-choice][value=reembed]'));
click($('confirm-ok'));
await tick(100);
assert.equal(activateBody.confirmations.embedder_compat, 'reembed');

// 7. static safety properties of the source
assert.ok(!/innerHTML|localStorage|sessionStorage/.test(html.replace(/<style[\s\S]*?<\/style>/, '')), 'no innerHTML/localStorage/sessionStorage');
console.log('plugins UI smoke test: all checks passed');
process.exit(0);
