// Headless Chromium checks of the workbench against a running abhed serve.
//
//   node checks.mjs BASE_URL WORKSPACE
//
// Each check prints PASS or FAIL with what it saw; the exit code is the
// number of failures. The server must have local accounts with sign-up open.
import fs from 'node:fs';
import path from 'node:path';
import {chromium} from 'playwright';

const [base, workspace] = process.argv.slice(2);
if (!base || !workspace) { console.error('usage: node checks.mjs BASE_URL WORKSPACE'); process.exit(2); }

let failures = 0;
const result = (name, ok, detail) => {
  console.log((ok ? 'PASS ' : 'FAIL ') + name + (detail ? ': ' + detail : ''));
  if (!ok) failures++;
};
const waitUntil = async (what, pred, ms = 15000) => {
  for (const end = Date.now() + ms; Date.now() < end; await new Promise(r => setTimeout(r, 100))) {
    if (await pred()) return true;
  }
  throw new Error('waited ' + ms + ' ms for ' + what);
};

const browser = await chromium.launch();
const context = await browser.newContext({viewport: {width: 1440, height: 900}});
// A long name, as an identity provider can give, is what pushes Sign out off a narrow header.
const user = 'workbench-layout-check-with-a-long-identity-name';
const password = 'correct-horse-battery-staple-9';
await context.request.post(base + '/v1/signup', {data: {username: user, password, email: user + '@example.org'}});
const signin = await context.request.post(base + '/v1/signin', {data: {username: user, password}});
if (!signin.ok()) { console.error('could not sign in: ' + signin.status() + ' ' + await signin.text()); process.exit(2); }
const page = await context.newPage();
page.on('pageerror', e => result('no script error on the page', false, e.message));

const box = sel => page.locator(sel).first().boundingBox();

// F15: at every width the header keeps Sign out on screen, inside its row,
// and the page never scrolls sideways.
for (const width of [320, 900, 1440]) {
  await page.setViewportSize({width, height: 800});
  await page.goto(base + '/ide');
  try {
    await page.locator('#signout').waitFor({state: 'visible', timeout: 15000});
  } catch {
    result(`F15 header at ${width}px`, false, 'Sign out never showed');
    continue;
  }
  const out = await box('#signout'), title = await box('#title'), right = await box('#title .right');
  const clipped = await page.evaluate(() => {
    const r = document.querySelector('#title .right'), s = document.querySelector('#signout');
    const rr = r.getBoundingClientRect(), sr = s.getBoundingClientRect();
    return sr.right > rr.right + 0.5 || sr.left < rr.left - 0.5;
  });
  const sideways = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth);
  const ok = out && out.width > 0 && out.x >= 0 && out.x + out.width <= width + 0.5 &&
    out.y >= title.y && out.y + out.height <= title.y + title.height + 0.5 && !clipped && !sideways;
  result(`F15 header at ${width}px`, ok,
    `sign out at x=${out && Math.round(out.x)} w=${out && Math.round(out.width)}, row right=${right && Math.round(right.x + right.width)}, clipped=${clipped}, sideways scroll=${sideways}`);
}

// F3: at 1440px the side-panel toggle collapses the side panel and the grid
// stays whole: the editor fills from the activity bar to the agent, which
// keeps the right edge; toggled back, the side panel returns.
await page.setViewportSize({width: 1440, height: 900});
await page.goto(base + '/ide');
await page.locator('#q').waitFor({state: 'visible'});
const layout = async () => ({
  act: await box('#act'), side: await box('#side'), center: await box('#center'), agent: await box('#agent'),
  noside: await page.evaluate(() => document.getElementById('app').classList.contains('noside')),
});
const before = await layout();
await page.locator('#center').click({position: {x: 5, y: 5}}).catch(() => {});
await page.keyboard.press('ControlOrMeta+b');
const collapsed = await layout();
const near = (a, b) => Math.abs(a - b) <= 6;
let ok = collapsed.noside && !collapsed.side && collapsed.center && collapsed.agent &&
  near(collapsed.center.x, collapsed.act.x + collapsed.act.width) &&
  near(collapsed.agent.x + collapsed.agent.width, 1440) &&
  collapsed.center.x + collapsed.center.width <= collapsed.agent.x + 0.5 &&
  collapsed.center.width > before.center.width;
result('F3 grid with the side panel collapsed at 1440px', ok, JSON.stringify(collapsed));
await page.keyboard.press('ControlOrMeta+b');
const back = await layout();
ok = !back.noside && back.side && back.side.width > 0 && near(back.center.x, before.center.x) &&
  near(back.agent.x, before.agent.x);
result('F3 grid with the side panel back at 1440px', ok, JSON.stringify(back));

// First-turn approval: the first call of a new session's first turn is put to
// the person in the workbench, and allowing it runs it.
const made = path.join(workspace, 'approved.txt');
fs.rmSync(made, {force: true});
await page.locator('#q').fill('make the file');
await page.locator('#send').click();
try {
  const ask = page.locator('.ask').filter({hasText: 'touch approved.txt'}).first();
  await ask.waitFor({state: 'visible', timeout: 20000});
  result('first-turn approval is asked', !fs.existsSync(made), fs.existsSync(made) ? 'the command ran before it was allowed' : '');
  await ask.locator('button.yes').click();
  await waitUntil('approved.txt', () => fs.existsSync(made));
  await page.locator('#tx').getByText('done', {exact: true}).first().waitFor({timeout: 15000});
  result('first-turn approval, once allowed, runs the call and the turn ends', true);
} catch (e) {
  result('first-turn approval', false, e.message);
}

await browser.close();
process.exit(failures);
