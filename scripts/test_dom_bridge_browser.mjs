import http from 'node:http';
import fs from 'node:fs/promises';
import path from 'node:path';

// 1. Read the injected inspector script directly from internal/api/preview_handlers.go
const goSourcePath = path.resolve('internal/api/preview_handlers.go');
const goSource = await fs.readFile(goSourcePath, 'utf8');

const match = goSource.match(/inspectorScript := fmt\.Sprintf\(`([\s\S]*?)`,\s*consoleOrigin\)/);
if (!match) {
  console.error('Failed to extract inspectorScript from preview_handlers.go');
  process.exit(1);
}

const rawInspectorScript = match[1];
// Two completely distinct Origins (different ports enforce browser cross-origin boundaries)
const CONSOLE_PORT = 28991;
const PREVIEW_PORT = 28992;
const CONSOLE_ORIGIN = `http://127.0.0.1:${CONSOLE_PORT}`;
const PREVIEW_ORIGIN = `http://127.0.0.1:${PREVIEW_PORT}`;

// Injected script in preview iframe is configured with expectedConsoleOrigin
const injectedScript = rawInspectorScript.replace('%q', JSON.stringify(CONSOLE_ORIGIN));

// 2. Transpile web/src/lib/domTarget.ts so test harness shares EXACT source code without duplication
const tsModule = await import(path.resolve('web/node_modules/typescript/lib/typescript.js'));
const ts = tsModule.default || tsModule;
const tsSource = await fs.readFile(path.resolve('web/src/lib/domTarget.ts'), 'utf8');
const { outputText: domTargetJs } = ts.transpileModule(tsSource, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 }
});

// 3. Start Console HTTP server (origin: CONSOLE_ORIGIN)
const consoleServer = http.createServer((req, res) => {
  const url = new URL(req.url, CONSOLE_ORIGIN);

  if (url.pathname === '/domTarget.js') {
    res.writeHead(200, { 'Content-Type': 'application/javascript; charset=utf-8' });
    res.end(domTargetJs);
    return;
  }

  if (url.pathname === '/parent') {
    res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
    res.end(`<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>PreviewDrawer Parent Harness</title>
  <style>
    * { margin: 0; padding: 0; box-sizing: border-box; }
    body { padding: 20px; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #f8fafc; }
    #preview-iframe {
      position: absolute;
      left: 20px;
      top: 160px;
      width: 600px;
      height: 540px;
      border: 2px solid #cbd5e1;
      background: #ffffff;
      border-radius: 6px;
    }
  </style>
  <script type="module">
    // Load authoritative decodeDOMTarget directly from transpiled domTarget.ts
    import { decodeDOMTarget } from '/domTarget.js';
    window.decodeDOMTarget = decodeDOMTarget;

    const previewOrigin = '${PREVIEW_ORIGIN}';
    window.testLogs = [];
    window.inspectorMounted = false;
    window.inspectorActive = false;
    window.lastDecoded = null;

    function recordLog(msg) {
      window.testLogs.push(msg);
      const logsEl = document.getElementById('logs');
      if (logsEl) {
        const p = document.createElement('div');
        p.textContent = msg;
        logsEl.appendChild(p);
      }
      console.log('[Parent Harness]', msg);
    }

    // Explicit cross-origin isolation assertion helper
    window.checkCrossOriginBlocked = function() {
      const iframe = document.getElementById('preview-iframe');
      let contentDocBlocked = false;
      let contentWinBlocked = false;
      try {
        const d = iframe.contentDocument;
        if (d === null) contentDocBlocked = true;
      } catch(e) {
        contentDocBlocked = true;
      }
      try {
        const loc = iframe.contentWindow.location.href;
        contentWinBlocked = false;
      } catch(e) {
        contentWinBlocked = true;
      }
      return { contentDocBlocked, contentWinBlocked };
    };

    window.addEventListener('message', (e) => {
      if (!e || !e.data || typeof e.data !== 'object') return;
      if (e.source === window) {
        // Self window message (extensions/CDP internal), ignore
        return;
      }
      const iframeEl = document.getElementById('preview-iframe');
      if (!previewOrigin || e.origin !== previewOrigin) {
        recordLog('REJECTED_ORIGIN: ' + e.origin);
        return;
      }
      if (!iframeEl || (e.source !== iframeEl.contentWindow && e.source !== window.frames[0])) {
        recordLog('REJECTED_SOURCE');
        return;
      }

      const t = e.data.type;
      if (t === 'MG_INSPECTOR_MOUNTED') {
        window.inspectorMounted = true;
        recordLog('MG_INSPECTOR_MOUNTED');
      } else if (t === 'MG_INSPECTOR_ACTIVE') {
        window.inspectorActive = true;
        recordLog('MG_INSPECTOR_ACTIVE');
      } else if (t === 'MG_INSPECTOR_INACTIVE') {
        window.inspectorActive = false;
        recordLog('MG_INSPECTOR_INACTIVE');
      } else if (t === 'MG_DOM_SELECTED' && e.data.target) {
        const decoded = decodeDOMTarget(e.data.target);
        if (decoded) {
          window.lastDecoded = decoded;
          const pill = document.getElementById('dom-pill');
          const pillText = document.getElementById('pill-text');
          if (pill) pill.style.display = 'inline-flex';
          const displayText = decoded.tag + (decoded.id ? '#' + decoded.id : '');
          if (pillText) pillText.textContent = displayText;
          recordLog('ACCEPTED: ' + displayText + ' (selector: ' + decoded.selector + ')');
        } else {
          recordLog('REJECTED_PAYLOAD: ' + JSON.stringify(e.data.target));
        }
      }
    });

    window.startInspector = function() {
      const iframeEl = document.getElementById('preview-iframe');
      if (iframeEl && iframeEl.contentWindow) {
        iframeEl.contentWindow.postMessage({ type: 'MG_START_INSPECTOR' }, previewOrigin);
        recordLog('POSTED_MG_START_INSPECTOR');
      }
    };
  </script>
</head>
<body>
  <h2>Parent PreviewDrawer Harness (Console Origin: ${CONSOLE_ORIGIN})</h2>
  <button id="btn-inspect" onclick="startInspector()" style="padding: 6px 12px; margin-top: 8px;">选元素</button>
  <div id="dom-pill" style="display:none; margin: 10px 0; padding: 4px 8px; border: 1px solid skyblue; background: #e0f2fe; border-radius: 4px;">
    🎯 @DOM <span id="pill-text" style="font-weight: bold; font-family: monospace;"></span>
  </div>
  <div id="logs" style="font-family: monospace; font-size: 11px; margin-top: 8px; color: #555; height: 50px; overflow-y: auto;"></div>
  <iframe id="preview-iframe" src="${PREVIEW_ORIGIN}/iframe"></iframe>
</body>
</html>`);
    return;
  }

  res.writeHead(404);
  res.end('Not Found');
});

// 4. Start Preview HTTP server (origin: PREVIEW_ORIGIN)
const previewServer = http.createServer((req, res) => {
  const url = new URL(req.url, PREVIEW_ORIGIN);

  if (url.pathname === '/iframe') {
    res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
    res.end(`<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>Injected Preview Document</title>
  ${injectedScript}
  <style>
    * { margin: 0; padding: 0; box-sizing: border-box; }
    body { padding: 20px; font-family: sans-serif; position: relative; width: 100%; height: 100%; background: #ffffff; }
    #save { position: absolute; left: 30px; top: 20px; width: 150px; height: 40px; background: #0284c7; color: white; border: none; border-radius: 6px; cursor: pointer; }
    .card { position: absolute; left: 30px; top: 80px; width: 300px; height: 50px; padding: 12px; border: 2px solid #6366f1; background: #eef2ff; border-radius: 6px; }
    p { position: absolute; left: 30px; top: 150px; width: 300px; height: 40px; color: #333; font-size: 16px; margin: 0; }
    #btn-forge-tag { position: absolute; left: 30px; top: 210px; width: 220px; height: 35px; background: #ef4444; color: white; border: none; border-radius: 4px; cursor: pointer; }
    #btn-forge-sel-id { position: absolute; left: 30px; top: 260px; width: 220px; height: 35px; background: #f97316; color: white; border: none; border-radius: 4px; cursor: pointer; }
    #btn-forge-sel-class { position: absolute; left: 30px; top: 310px; width: 220px; height: 35px; background: #eab308; color: white; border: none; border-radius: 4px; cursor: pointer; }
  </style>
</head>
<body>
  <button id="save" class="btn btn-primary" role="button" data-testid="save-button">Save Changes</button>
  <div class="card" role="region" data-testid="main-card">Card Component with class</div>
  <p>Plain text paragraph with no ID</p>

  <!-- Native buttons inside child origin used to emit forged payloads to parent -->
  <button id="btn-forge-tag" onclick="window.parent.postMessage({ type: 'MG_DOM_SELECTED', target: { tag: 'button#secret', tagName: 'button', selector: 'button:nth-of-type(1)' } }, '${CONSOLE_ORIGIN}')">Forge Tag: button#secret</button>
  <button id="btn-forge-sel-id" onclick="window.parent.postMessage({ type: 'MG_DOM_SELECTED', target: { tag: 'button', tagName: 'button', selector: 'button#save' } }, '${CONSOLE_ORIGIN}')">Forge Sel: #id</button>
  <button id="btn-forge-sel-class" onclick="window.parent.postMessage({ type: 'MG_DOM_SELECTED', target: { tag: 'div', tagName: 'div', selector: 'div.card' } }, '${CONSOLE_ORIGIN}')">Forge Sel: .class</button>
</body>
</html>`);
    return;
  }

  res.writeHead(404);
  res.end('Not Found');
});

await Promise.all([
  new Promise((resolve) => consoleServer.listen(CONSOLE_PORT, '127.0.0.1', resolve)),
  new Promise((resolve) => previewServer.listen(PREVIEW_PORT, '127.0.0.1', resolve)),
]);
console.log(`Console server running at ${CONSOLE_ORIGIN}`);
console.log(`Preview server running at ${PREVIEW_ORIGIN}`);

// 3. Launch ego-browser automation script to execute the browser regression
// 5. Automation script for ego-browser
const egoScript = `
const task = await taskSpace("preview-dom-bridge-regression");
const page = task.page("p1");
await page.goto("${CONSOLE_ORIGIN}/parent");

async function poll(fn, timeoutMs = 4000) {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    const ok = await page.evaluate(fn);
    if (ok) return ok;
    await new Promise(r => setTimeout(r, 100));
  }
  const logs = await page.evaluate(() => window.testLogs);
  console.error("Test logs on timeout:", logs);
  throw new Error("Polling timeout for: " + fn.toString());
}

// 0. Wait for child iframe to mount and post MG_INSPECTOR_MOUNTED via cross-origin bridge
await poll(() => window.inspectorMounted === true);
console.log("Iframe mounted and bridge communication established.");

// 0.1 Explicitly assert cross-origin isolation
const crossOriginCheck = await page.evaluate(() => window.checkCrossOriginBlocked());
console.log("Cross-origin boundary assertion:", JSON.stringify(crossOriginCheck));
if (!crossOriginCheck.contentDocBlocked || !crossOriginCheck.contentWinBlocked) {
  throw new Error("Cross-origin security boundary failed: iframe contentDocument or window was accessible!");
}
console.log("PASS: Cross-origin security boundary verified (contentDocument and window access strictly blocked).");

// 1. Test button#save via Chromium native mouse input (iframe: left=20, top=160; save: left=30, top=20, w=150, h=40 -> center=(125, 200))
console.log("--- TEST 1: Click button#save inside iframe via page.mouse.click ---");
await page.evaluate(() => window.startInspector());
await poll(() => window.inspectorActive === true);

await page.mouse.move(125, 200);
await page.mouse.click(125, 200);

await poll(() => document.getElementById("pill-text")?.textContent === "button#save");
const decoded1 = await page.evaluate(() => window.lastDecoded);
console.log("PASS TEST 1: pill displays button#save, decoded:", JSON.stringify(decoded1));
if (decoded1.tag !== "button" || decoded1.tagName !== "button" || decoded1.id !== "save") {
  throw new Error("TEST 1 FAILED: expected tag=button, tagName=button, id=save, got " + JSON.stringify(decoded1));
}

// 2. Test div.card via Chromium native mouse input (card: left=30, top=80, w=300, h=50 -> center=(200, 265))
console.log("--- TEST 2: Click div.card inside iframe via page.mouse.click ---");
await page.evaluate(() => window.startInspector());
await poll(() => window.inspectorActive === true);

await page.mouse.move(200, 265);
await page.mouse.click(200, 265);

await poll(() => document.getElementById("pill-text")?.textContent === "div");
const decoded2 = await page.evaluate(() => window.lastDecoded);
console.log("PASS TEST 2: pill displays div, decoded:", JSON.stringify(decoded2));
if (decoded2.tag !== "div" || decoded2.tagName !== "div" || decoded2.id !== undefined) {
  throw new Error("TEST 2 FAILED: expected tag=div, tagName=div, id=undefined, got " + JSON.stringify(decoded2));
}

// 3. Test plain <p> via Chromium native mouse input (para: left=30, top=150, w=300, h=40 -> center=(200, 330))
console.log("--- TEST 3: Click plain <p> inside iframe via page.mouse.click ---");
await page.evaluate(() => window.startInspector());
await poll(() => window.inspectorActive === true);

await page.mouse.move(200, 330);
await page.mouse.click(200, 330);

await poll(() => document.getElementById("pill-text")?.textContent === "p");
const decoded3 = await page.evaluate(() => window.lastDecoded);
console.log("PASS TEST 3: pill displays p, decoded:", JSON.stringify(decoded3));
if (decoded3.tag !== "p" || decoded3.tagName !== "p") {
  throw new Error("TEST 3 FAILED: expected tag=p, tagName=p, got " + JSON.stringify(decoded3));
}

// 4. Forged payload: tag: 'button#secret' emitted natively by clicking child button (center=(160, 387))
console.log("--- TEST 4: Forged payload tag: button#secret ---");
await page.mouse.move(160, 387);
await page.mouse.click(160, 387);
await new Promise(r => setTimeout(r, 300));
const pillTextAfterForgedTag = await page.evaluate(() => document.getElementById("pill-text")?.textContent);
console.log("Pill text after forged tag:", pillTextAfterForgedTag);
if (pillTextAfterForgedTag !== "p") {
  throw new Error("TEST 4 FAILED: forged tag 'button#secret' was NOT dropped! pillText=" + pillTextAfterForgedTag);
}
console.log("PASS TEST 4: forged tag 'button#secret' successfully rejected fail-closed");

// 5. Forged selector containing id: selector: 'button#save' (center=(160, 437))
console.log("--- TEST 5: Forged selector containing #id ---");
await page.mouse.move(160, 437);
await page.mouse.click(160, 437);
await new Promise(r => setTimeout(r, 300));
const pillTextAfterForgedSel1 = await page.evaluate(() => document.getElementById("pill-text")?.textContent);
if (pillTextAfterForgedSel1 !== "p") {
  throw new Error("TEST 5 FAILED: forged selector 'button#save' was NOT dropped! pillText=" + pillTextAfterForgedSel1);
}
console.log("PASS TEST 5: forged selector with #id successfully rejected fail-closed");

// 6. Forged selector containing class: selector: 'div.card' (center=(160, 487))
console.log("--- TEST 6: Forged selector containing .class ---");
await page.mouse.move(160, 487);
await page.mouse.click(160, 487);
await new Promise(r => setTimeout(r, 300));
const pillTextAfterForgedSel2 = await page.evaluate(() => document.getElementById("pill-text")?.textContent);
if (pillTextAfterForgedSel2 !== "p") {
  throw new Error("TEST 6 FAILED: forged selector 'div.card' was NOT dropped! pillText=" + pillTextAfterForgedSel2);
}
console.log("PASS TEST 6: forged selector with .class successfully rejected fail-closed");

// 7. Forged message from untrusted origin (simulated via untrusted data URI iframe)
console.log("--- TEST 7: Untrusted origin message ---");
await page.evaluate(() => {
  const badFrame = document.createElement('iframe');
  badFrame.id = 'untrusted-attacker-iframe';
  badFrame.style.display = 'none';
  badFrame.src = 'data:text/html,<script>window.parent.postMessage({ type: "MG_DOM_SELECTED", target: { tag: "button", tagName: "button", selector: "button:nth-of-type(1)" } }, "*");<\/script>';
  document.body.appendChild(badFrame);
});
await poll(() => window.testLogs && window.testLogs.some(l => l.startsWith("REJECTED_ORIGIN: null")));
console.log("PASS TEST 7: untrusted origin postMessage rejected fail-closed");

console.log("ALL 7 CROSS-ORIGIN BROWSER REGRESSION TESTS PASSED!");
await task.finish({ keep: [] });
`;

// Spawn ego-browser nodejs with stdin
const { spawn } = await import('node:child_process');
const proc = spawn('ego-browser', ['nodejs'], { stdio: ['pipe', 'inherit', 'inherit'] });
proc.stdin.write(egoScript);
proc.stdin.end();

proc.on('close', async (code) => {
  consoleServer.close();
  previewServer.close();
  if (code !== 0) {
    console.error(`ego-browser exited with code ${code}`);
    process.exit(code || 1);
  }
  console.log('True cross-origin browser regression suite completed successfully.');
  process.exit(0);
});
