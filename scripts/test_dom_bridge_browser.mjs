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
const PORT = 28991;
const CONSOLE_ORIGIN = `http://127.0.0.1:${PORT}`;
const PREVIEW_ORIGIN = `http://127.0.0.1:${PORT}`;

// Format template with console origin (Go uses %q which adds double quotes)
const injectedScript = rawInspectorScript.replace('%q', JSON.stringify(CONSOLE_ORIGIN));

// 2. Start HTTP server
const server = http.createServer((req, res) => {
  const url = new URL(req.url, `http://${req.headers.host}`);
  
  if (url.pathname === '/parent') {
    res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
    res.end(`<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>PreviewDrawer Parent Harness</title>
  <script type="module">
    // EXACT decodeDOMTarget from PreviewDrawer.tsx
    const SAFE_TAG_REGEX = /^[a-z][a-z0-9-]{0,31}$/;
    const SAFE_ID_REGEX = /^[A-Za-z][A-Za-z0-9_:-]{0,63}$/;
    const SAFE_ROLE_TYPE_REGEX = /^[a-zA-Z0-9_\\-]{1,32}$/;
    const SAFE_SELECTOR_SEGMENT_REGEX = /^[a-z][a-z0-9-]{0,31}(?::nth-of-type\\(\\d+\\))?$/;

    export function decodeDOMTarget(raw) {
      if (!raw || typeof raw !== 'object') return null;
      const obj = raw;

      const rawTagName = typeof obj.tagName === 'string' && obj.tagName.trim()
        ? obj.tagName.trim().toLowerCase()
        : (typeof obj.tag === 'string' ? obj.tag.trim().toLowerCase() : '');

      if (!SAFE_TAG_REGEX.test(rawTagName)) {
        return null;
      }

      if (typeof obj.tag === 'string') {
        const rawTag = obj.tag.trim().toLowerCase();
        if (!SAFE_TAG_REGEX.test(rawTag)) {
          return null;
        }
      }

      const selector = typeof obj.selector === 'string' ? obj.selector.trim() : '';
      if (!selector) return null;
      const segments = selector.split(' > ');
      if (segments.length === 0 || segments.length > 20) return null;
      for (const seg of segments) {
        if (!SAFE_SELECTOR_SEGMENT_REGEX.test(seg)) {
          return null;
        }
      }

      const result = {
        tag: rawTagName,
        tagName: rawTagName,
        selector,
      };

      if (typeof obj.id === 'string' && obj.id.trim()) {
        const cleanId = obj.id.trim();
        if (SAFE_ID_REGEX.test(cleanId)) {
          result.id = cleanId;
        }
      }

      if (typeof obj.role === 'string' && obj.role.trim()) {
        const cleanRole = obj.role.trim();
        if (SAFE_ROLE_TYPE_REGEX.test(cleanRole)) {
          result.role = cleanRole;
        }
      }

      if (typeof obj.type === 'string' && obj.type.trim()) {
        const cleanType = obj.type.trim();
        if (SAFE_ROLE_TYPE_REGEX.test(cleanType)) {
          result.type = cleanType;
        }
      }

      if (typeof obj.testId === 'string' && obj.testId.trim()) {
        const cleanTestId = obj.testId.trim();
        if (SAFE_ROLE_TYPE_REGEX.test(cleanTestId)) {
          result.testId = cleanTestId;
        }
      }

      return result;
    }

    window.decodeDOMTarget = decodeDOMTarget;

    const previewOrigin = '${PREVIEW_ORIGIN}';
    window.testLogs = [];
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

    window.addEventListener('message', (e) => {
      if (!e || !e.data || typeof e.data !== 'object') return;
      const iframeEl = document.getElementById('preview-iframe');
      if (!previewOrigin || e.origin !== previewOrigin) {
        recordLog('REJECTED_ORIGIN: ' + e.origin);
        return;
      }
      if (!iframeEl || (e.source !== iframeEl.contentWindow && e.source !== window.frames[0])) {
        if (e.source === window) {
          // Message from self (e.g. ego-browser internal / extensions), ignore silently
          return;
        }
        recordLog('REJECTED_SOURCE: isContentWin=' + (e.source === iframeEl.contentWindow) + ' isFrames0=' + (e.source === window.frames[0]) + ' isSelf=' + (e.source === window) + ' data=' + JSON.stringify(e.data));
        return;
      }

      const t = e.data.type;
      if (t === 'MG_DOM_SELECTED' && e.data.target) {
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
<body style="padding: 20px; font-family: sans-serif;">
  <h2>Parent PreviewDrawer Harness</h2>
  <button id="btn-inspect" onclick="startInspector()">选元素</button>
  <div id="dom-pill" style="display:none; margin: 10px 0; padding: 4px 8px; border: 1px solid skyblue; background: #e0f2fe; border-radius: 4px;">
    🎯 @DOM <span id="pill-text" style="font-weight: bold; font-family: monospace;"></span>
  </div>
  <div id="logs" style="font-family: monospace; font-size: 11px; margin-top: 10px; color: #555;"></div>
  <iframe id="preview-iframe" src="/iframe" style="width: 100%; height: 350px; border: 1px solid #ccc; margin-top: 10px;"></iframe>
</body>
</html>`);
    return;
  }

  if (url.pathname === '/iframe') {
    res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
    res.end(`<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>Injected Preview Document</title>
  ${injectedScript}
  <style>
    body { padding: 30px; font-family: sans-serif; }
    .card { padding: 20px; border: 2px solid #6366f1; background: #eef2ff; margin: 15px 0; }
    .btn { padding: 10px 20px; background: #0284c7; color: white; border: none; border-radius: 6px; cursor: pointer; }
    p { color: #333; font-size: 16px; }
  </style>
</head>
<body>
  <h3>Preview Sandbox Content</h3>
  <button id="save" class="btn btn-primary" role="button" data-testid="save-button">Save Changes</button>
  <div class="card" role="region" data-testid="main-card">Card Component with class</div>
  <p>Plain text paragraph with no ID</p>
</body>
</html>`);
    return;
  }

  res.writeHead(404);
  res.end('Not Found');
});

await new Promise((resolve) => server.listen(PORT, '127.0.0.1', resolve));
console.log(`Test server running at http://127.0.0.1:${PORT}`);

// 3. Launch ego-browser automation script to execute the browser regression
const egoScript = `
const task = await taskSpace("preview-dom-bridge-regression");
const page = task.page("p1");
await page.goto("${CONSOLE_ORIGIN}/parent");

// Helper to wait for predicate in page
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

// Wait for iframe to load completely
await poll(() => {
  const iframe = document.getElementById("preview-iframe");
  return iframe && iframe.contentDocument && iframe.contentDocument.readyState === "complete";
});
console.log("Iframe loaded completely.");

// 1. Test button#save
console.log("--- TEST 1: Click button#save inside iframe ---");
await page.evaluate(() => {
  window.startInspector();
});
await new Promise(r => setTimeout(r, 200));

// Click button#save in iframe
await page.evaluate(() => {
  const iframe = document.getElementById("preview-iframe");
  const doc = iframe.contentDocument;
  const btn = doc.getElementById("save");
  const rect = btn.getBoundingClientRect();
  const x = rect.left + rect.width / 2;
  const y = rect.top + rect.height / 2;
  doc.dispatchEvent(new MouseEvent("mousemove", { clientX: x, clientY: y }));
  btn.dispatchEvent(new MouseEvent("click", { clientX: x, clientY: y, bubbles: true }));
});

await poll(() => document.getElementById("pill-text")?.textContent === "button#save");
const decoded1 = await page.evaluate(() => window.lastDecoded);
console.log("PASS TEST 1: pill displays button#save, decoded:", JSON.stringify(decoded1));
if (decoded1.tag !== "button" || decoded1.tagName !== "button" || decoded1.id !== "save") {
  throw new Error("TEST 1 FAILED: expected tag=button, tagName=button, id=save, got " + JSON.stringify(decoded1));
}

// 2. Test div.card
console.log("--- TEST 2: Click div.card inside iframe ---");
await page.evaluate(() => {
  window.startInspector();
});
await new Promise(r => setTimeout(r, 200));

await page.evaluate(() => {
  const iframe = document.getElementById("preview-iframe");
  const doc = iframe.contentDocument;
  const div = doc.querySelector("div.card");
  const rect = div.getBoundingClientRect();
  const x = rect.left + rect.width / 2;
  const y = rect.top + rect.height / 2;
  doc.dispatchEvent(new MouseEvent("mousemove", { clientX: x, clientY: y }));
  div.dispatchEvent(new MouseEvent("click", { clientX: x, clientY: y, bubbles: true }));
});

await poll(() => document.getElementById("pill-text")?.textContent === "div");
const decoded2 = await page.evaluate(() => window.lastDecoded);
console.log("PASS TEST 2: pill displays div, decoded:", JSON.stringify(decoded2));
if (decoded2.tag !== "div" || decoded2.tagName !== "div" || decoded2.id !== undefined) {
  throw new Error("TEST 2 FAILED: expected tag=div, tagName=div, id=undefined, got " + JSON.stringify(decoded2));
}

// 3. Test plain <p>
console.log("--- TEST 3: Click plain <p> inside iframe ---");
await page.evaluate(() => {
  window.startInspector();
});
await new Promise(r => setTimeout(r, 200));

await page.evaluate(() => {
  const iframe = document.getElementById("preview-iframe");
  const doc = iframe.contentDocument;
  const p = doc.querySelector("p");
  const rect = p.getBoundingClientRect();
  const x = rect.left + rect.width / 2;
  const y = rect.top + rect.height / 2;
  doc.dispatchEvent(new MouseEvent("mousemove", { clientX: x, clientY: y }));
  p.dispatchEvent(new MouseEvent("click", { clientX: x, clientY: y, bubbles: true }));
});

await poll(() => document.getElementById("pill-text")?.textContent === "p");
const decoded3 = await page.evaluate(() => window.lastDecoded);
console.log("PASS TEST 3: pill displays p, decoded:", JSON.stringify(decoded3));
if (decoded3.tag !== "p" || decoded3.tagName !== "p") {
  throw new Error("TEST 3 FAILED: expected tag=p, tagName=p, got " + JSON.stringify(decoded3));
}

// 4. Forged payload: tag: 'button#secret'
console.log("--- TEST 4: Forged payload tag: button#secret ---");
await page.evaluate(() => {
  const iframe = document.getElementById("preview-iframe");
  iframe.contentWindow.postMessage({
    type: "MG_DOM_SELECTED",
    target: { tag: "button#secret", tagName: "button", selector: "button:nth-of-type(1)" }
  }, "${PREVIEW_ORIGIN}");
});
await new Promise(r => setTimeout(r, 300));
const pillTextAfterForgedTag = await page.evaluate(() => document.getElementById("pill-text")?.textContent);
console.log("Pill text after forged tag:", pillTextAfterForgedTag);
if (pillTextAfterForgedTag !== "p") {
  throw new Error("TEST 4 FAILED: forged tag 'button#secret' was NOT dropped! pillText=" + pillTextAfterForgedTag);
}
console.log("PASS TEST 4: forged tag 'button#secret' successfully rejected fail-closed");

// 5. Forged selector containing id: selector: 'button#save'
console.log("--- TEST 5: Forged selector containing #id ---");
await page.evaluate(() => {
  const iframe = document.getElementById("preview-iframe");
  iframe.contentWindow.postMessage({
    type: "MG_DOM_SELECTED",
    target: { tag: "button", tagName: "button", selector: "button#save" }
  }, "${PREVIEW_ORIGIN}");
});
await new Promise(r => setTimeout(r, 300));
const pillTextAfterForgedSel1 = await page.evaluate(() => document.getElementById("pill-text")?.textContent);
if (pillTextAfterForgedSel1 !== "p") {
  throw new Error("TEST 5 FAILED: forged selector 'button#save' was NOT dropped! pillText=" + pillTextAfterForgedSel1);
}
console.log("PASS TEST 5: forged selector with #id successfully rejected fail-closed");

// 6. Forged selector containing class: selector: 'div.card'
console.log("--- TEST 6: Forged selector containing .class ---");
await page.evaluate(() => {
  const iframe = document.getElementById("preview-iframe");
  iframe.contentWindow.postMessage({
    type: "MG_DOM_SELECTED",
    target: { tag: "div", tagName: "div", selector: "div.card" }
  }, "${PREVIEW_ORIGIN}");
});
await new Promise(r => setTimeout(r, 300));
const pillTextAfterForgedSel2 = await page.evaluate(() => document.getElementById("pill-text")?.textContent);
if (pillTextAfterForgedSel2 !== "p") {
  throw new Error("TEST 6 FAILED: forged selector 'div.card' was NOT dropped! pillText=" + pillTextAfterForgedSel2);
}
console.log("PASS TEST 6: forged selector with .class successfully rejected fail-closed");

console.log("ALL 6 BROWSER REGRESSION TESTS PASSED!");
await task.finish({ keep: [] });
`;

// Spawn ego-browser nodejs with stdin
const { spawn } = await import('node:child_process');
const proc = spawn('ego-browser', ['nodejs'], { stdio: ['pipe', 'inherit', 'inherit'] });
proc.stdin.write(egoScript);
proc.stdin.end();

proc.on('close', async (code) => {
  server.close();
  try { await fs.unlink('scripts/run_ego_regression.js'); } catch {}
  if (code !== 0) {
    console.error(`ego-browser exited with code ${code}`);
    process.exit(code || 1);
  }
  console.log('Browser regression suite completed successfully.');
  process.exit(0);
});
