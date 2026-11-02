// SPDX-License-Identifier: Apache-2.0
// Real compiled server + browser; no OpenAI credentials or inference requests.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtemp, readFile, stat } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { resolve, join } from 'node:path';
import { createRequire } from 'node:module';
import { createServer } from 'node:net';
import { setTimeout as sleep } from 'node:timers/promises';
const require = createRequire(join(process.env.PLAYWRIGHT_HOME || process.cwd(), 'package.json'));
const { chromium } = require('playwright');
const dir = await mkdtemp(join(tmpdir(), 'amplio-onboarding-'));
const binary = resolve(process.env.AMPLIO_BINARY || './amplio');
const socket = createServer(); await new Promise(r => socket.listen(0, '127.0.0.1', r));
const port = socket.address().port; await new Promise(r => socket.close(r));
const base = `http://127.0.0.1:${port}`;
let child, log = '', browser;
async function start() {
 log = '';
 const env = { ...process.env, AMPLIO_LISTEN: `127.0.0.1:${port}`, HOME: dir };
 for (const key of ['OPENAI_API_KEY','AMPLIO_OPENAI_KEY_FILE','AMPLIO_SYSTEM_LLM_HQ','AMPLIO_SYSTEM_LLM_FAST','AMPLIO_DATA_DIR']) delete env[key];
 child = spawn(binary, ['--data-dir', dir], {env, stdio: ['ignore','pipe','pipe']});
 child.stdout.on('data', b => {log += b;}); child.stderr.on('data', b => {log += b;});
 for (let i = 0; i < 160; i++) {
  if (child.exitCode !== null) throw new Error(`Server exited ${child.exitCode}: ${log}`);
  try { if ((await fetch(`${base}/api/auth`)).ok) return; } catch {}
  await sleep(100);
 }
 throw new Error(`Server startup timed out: ${log}`);
}
async function stop() {
 if (!child || child.exitCode !== null) return;
 const ended = new Promise(r => child.once('exit', r)); child.kill('SIGINT');
 await Promise.race([ended, sleep(10000).then(() => { if (child.exitCode === null) child.kill('SIGKILL'); })]);
}
try {
 await start();
 const token = (await readFile(join(dir,'auth.token'),'utf8')).trim();
 browser = await chromium.launch({headless:true});
 const context = await browser.newContext(); const page = await context.newPage();
 const errors=[];page.on('pageerror', e => errors.push(String(e)));
 // Only catalog transport is mocked at the browser boundary. Storage, owner
 // authorization, setup state, routing and defaults use the real Go server.
 await page.route('**/api/providers/models?*', r => r.fulfill({status:200,contentType:'application/json',body:JSON.stringify({models:[{id:'offline-model',name:'Offline test model'}]})}));
 await page.goto(`${base}/?token=${encodeURIComponent(token)}`);
 await page.waitForURL('**/providers');
 await page.getByRole('heading',{name:'Welcome to Amplio'}).waitFor();
 const secret='sk-offline-example-never-a-real-key';
 await page.getByLabel('API key',{exact:true}).fill(secret);
 await page.getByRole('button',{name:'Save API key',exact:true}).click();
 await page.waitForFunction(()=>document.querySelector('#agent-provider')?.value === 'openai');
 await page.getByLabel('Main agent model',{exact:true}).fill('offline-model');
 await page.getByRole('button',{name:'Save and start using Amplio'}).click();
 await page.waitForURL(`${base}/`);
 let saved = JSON.parse(await readFile(join(dir,'providers.json'),'utf8'));
 assert.deepEqual(saved,{agent:'openai-responses:offline-model',hq:'openai-responses:offline-model',fast:'openai-responses:offline-model'});
 assert.equal((await stat(join(dir,'credentials','openai_api_key'))).mode & 0o777,0o600);
 assert.equal((await stat(join(dir,'credentials'))).mode & 0o777,0o700);
 const status = await page.evaluate(async()=>await (await fetch('/api/providers')).json());
 assert.equal(status.setup_required,false);assert.equal(JSON.stringify(status).includes(secret),false);assert.equal(log.includes(secret),false);
 await page.goto(`${base}/providers`);
 await page.getByLabel('Use different models for reviews and summaries').check();
 await page.getByLabel('Review & reports model',{exact:true}).fill('offline-review');
 await page.getByLabel('Summaries & compaction model',{exact:true}).fill('offline-fast');
 await page.getByRole('button',{name:'Save defaults',exact:true}).click();
 await page.getByRole('status').filter({hasText:'Defaults saved'}).waitFor();
 saved=JSON.parse(await readFile(join(dir,'providers.json'),'utf8'));
 assert.equal(saved.hq,'openai-responses:offline-review');assert.equal(saved.fast,'openai-responses:offline-fast');
 await stop(); await start();
 await page.goto(`${base}/?token=${encodeURIComponent(token)}`);
 await page.waitForTimeout(700); assert.equal(new URL(page.url()).pathname,'/');
 await page.goto(`${base}/providers`);
 await page.getByRole('button',{name:'Save defaults',exact:true}).waitFor();
 assert.equal(await page.getByLabel('Review & reports model',{exact:true}).inputValue(),'offline-review');
 assert.equal(await page.getByLabel('API key',{exact:true}).inputValue(),'');
 await page.setViewportSize({width:390,height:844});
 await page.goto(`${base}/connections`);await page.waitForURL('**/providers');
 const width = await page.evaluate(()=>({body:document.body.scrollWidth,viewport:innerWidth}));
 assert.ok(width.body<=width.viewport+2,JSON.stringify(width));
 const anon=await browser.newContext();const viewer=await anon.newPage();await viewer.goto(`${base}/providers`);
 await viewer.getByRole('heading',{name:'Owner access required'}).waitFor();
 assert.equal((await viewer.request.get(`${base}/api/providers`)).status(),401);assert.deepEqual(errors,[]);
 await page.screenshot({path:process.env.AMPLIO_UI_SCREENSHOT || 'providers-mobile.png',fullPage:true});
 await anon.close();await context.close();
 console.log('PASS: blank-directory launch, onboarding, native key storage, live defaults, restart persistence, owner boundary, mobile layout and legacy-route redirect. No OpenAI calls.');
} catch (e) { console.error(log);throw e; }
finally { if(browser)await browser.close();await stop(); }
