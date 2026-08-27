/**
 * Multigent In-Context Feedback & Agent Assistant Widget
 * Injected automatically into ephemeral preview environments.
 */
(function () {
  if (window.__MULTIGENT_FEEDBACK_INITIALIZED__) return;
  window.__MULTIGENT_FEEDBACK_INITIALIZED__ = true;

  // Extract metadata from script tag
  var currentScript = document.currentScript || document.querySelector('script[data-task-id]');
  var taskId = (currentScript && currentScript.getAttribute('data-task-id')) || '';
  var project = (currentScript && currentScript.getAttribute('data-project')) || '';

  if (!taskId) {
    var match = window.location.pathname.match(/\/preview\/([^\/]+)/);
    if (match) taskId = match[1];
  }

  // Inject CSS styles
  var style = document.createElement('style');
  style.textContent = `
    .mg-feedback-root {
      position: fixed;
      bottom: 24px;
      right: 24px;
      z-index: 9999999;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      user-select: none;
      -webkit-user-select: none;
      touch-action: none;
    }
    .mg-feedback-pill-wrap {
      position: relative;
      display: inline-flex;
      align-items: center;
    }
    .mg-feedback-pill {
      display: inline-flex;
      align-items: center;
      gap: 7px;
      padding: 7px 13px;
      background: rgba(15, 23, 42, 0.88);
      color: #f8fafc;
      border: 1px solid rgba(255, 255, 255, 0.15);
      border-radius: 9999px;
      box-shadow: 0 8px 24px -4px rgba(0, 0, 0, 0.28), 0 0 0 1px rgba(255, 255, 255, 0.05);
      font-size: 12px;
      font-weight: 500;
      cursor: grab;
      backdrop-filter: blur(12px);
      -webkit-backdrop-filter: blur(12px);
      transition: background 0.15s ease, box-shadow 0.15s ease, transform 0.15s ease;
    }
    .mg-feedback-pill:hover {
      background: rgba(15, 23, 42, 0.96);
      box-shadow: 0 12px 28px -4px rgba(0, 0, 0, 0.35);
      transform: translateY(-1px);
    }
    .mg-feedback-pill:active {
      cursor: grabbing;
      transform: scale(0.98);
    }
    .mg-feedback-dot {
      width: 7px;
      height: 7px;
      border-radius: 50%;
      background: #10b981;
      box-shadow: 0 0 8px #10b981;
      flex-shrink: 0;
    }
    .mg-feedback-pill-text {
      letter-spacing: 0.01em;
    }
    .mg-feedback-kbd-hint {
      font-size: 10px;
      color: #94a3b8;
      background: rgba(255, 255, 255, 0.12);
      border-radius: 4px;
      padding: 1px 4px;
      margin-left: 2px;
      font-family: ui-monospace, monospace;
    }
    .mg-feedback-dismiss-btn {
      position: absolute;
      top: -6px;
      right: -6px;
      width: 16px;
      height: 16px;
      border-radius: 50%;
      background: #475569;
      color: #ffffff;
      border: 1px solid rgba(255, 255, 255, 0.3);
      display: none;
      align-items: center;
      justify-content: center;
      font-size: 10px;
      cursor: pointer;
      line-height: 1;
      transition: background 0.15s;
    }
    .mg-feedback-pill-wrap:hover .mg-feedback-dismiss-btn {
      display: flex;
    }
    .mg-feedback-dismiss-btn:hover {
      background: #ef4444;
    }
    .mg-feedback-drawer {
      position: absolute;
      bottom: 44px;
      right: 0;
      width: 360px;
      max-width: calc(100vw - 32px);
      background: #ffffff;
      border: 1px solid #e2e8f0;
      border-radius: 14px;
      box-shadow: 0 20px 40px -10px rgba(15, 23, 42, 0.2), 0 0 0 1px rgba(15, 23, 42, 0.05);
      padding: 16px 18px;
      display: none;
      flex-direction: column;
      gap: 12px;
      z-index: 10;
      animation: mgFeedbackFadeIn 0.2s cubic-bezier(0.16, 1, 0.3, 1);
    }
    @keyframes mgFeedbackFadeIn {
      from { opacity: 0; transform: translateY(8px) scale(0.97); }
      to { opacity: 1; transform: translateY(0) scale(1); }
    }
    .mg-feedback-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
    }
    .mg-feedback-title {
      font-size: 14px;
      font-weight: 700;
      color: #0f172a;
    }
    .mg-feedback-close {
      background: none;
      border: none;
      color: #94a3b8;
      cursor: pointer;
      font-size: 17px;
      line-height: 1;
      padding: 2px 6px;
      border-radius: 4px;
    }
    .mg-feedback-close:hover {
      color: #334155;
      background: #f1f5f9;
    }
    .mg-feedback-sub {
      font-size: 11.5px;
      color: #64748b;
      margin-top: -6px;
    }
    .mg-feedback-chips {
      display: flex;
      flex-wrap: wrap;
      gap: 5px;
    }
    .mg-feedback-chip {
      padding: 3px 9px;
      background: #f8fafc;
      border: 1px solid #e2e8f0;
      border-radius: 9999px;
      font-size: 11px;
      color: #475569;
      cursor: pointer;
      transition: all 0.15s ease;
    }
    .mg-feedback-chip:hover {
      background: #e0f2fe;
      color: #0284c7;
      border-color: #bae6fd;
    }
    .mg-feedback-textarea {
      width: 100%;
      height: 80px;
      box-sizing: border-box;
      border: 1px solid #cbd5e1;
      border-radius: 8px;
      padding: 8px 10px;
      font-size: 12.5px;
      color: #1e293b;
      outline: none;
      resize: vertical;
      font-family: inherit;
    }
    .mg-feedback-textarea:focus {
      border-color: #0284c7;
      box-shadow: 0 0 0 3px rgba(2, 132, 199, 0.12);
    }
    .mg-feedback-submit {
      width: 100%;
      padding: 8px;
      background: #0284c7;
      color: #ffffff;
      border: none;
      border-radius: 8px;
      font-size: 12.5px;
      font-weight: 600;
      cursor: pointer;
      transition: background 0.15s ease;
    }
    .mg-feedback-submit:hover {
      background: #0369a1;
    }
    .mg-feedback-submit:disabled {
      opacity: 0.6;
      cursor: not-allowed;
    }
    .mg-feedback-status {
      font-size: 11.5px;
      text-align: center;
      color: #059669;
      font-weight: 500;
    }
  `;
  document.head.appendChild(style);

  // Build DOM Structure
  var root = document.createElement('div');
  root.className = 'mg-feedback-root';

  var pillWrap = document.createElement('div');
  pillWrap.className = 'mg-feedback-pill-wrap';

  var pill = document.createElement('div');
  pill.className = 'mg-feedback-pill';
  var isMac = /Mac|iPod|iPhone|iPad/.test(navigator.platform);
  var kbdText = isMac ? '⌘K' : 'Ctrl+K';
  pill.innerHTML = '<span class="mg-feedback-dot"></span><span class="mg-feedback-pill-text">智能助手</span><span class="mg-feedback-kbd-hint">' + kbdText + '</span>';

  var dismissBtn = document.createElement('button');
  dismissBtn.className = 'mg-feedback-dismiss-btn';
  dismissBtn.innerHTML = '&times;';
  dismissBtn.title = '隐藏悬浮球 (' + kbdText + ' 重新呼出)';

  var drawer = document.createElement('div');
  drawer.className = 'mg-feedback-drawer';
  drawer.innerHTML = `
    <div class="mg-feedback-header">
      <div class="mg-feedback-title">边看边改</div>
      <button class="mg-feedback-close">&times;</button>
    </div>
    <div class="mg-feedback-sub">针对当前分支实时修改 · 快捷键 ${kbdText} 快速唤出</div>
    <div class="mg-feedback-chips">
      <span class="mg-feedback-chip" data-text="修复页面样式与布局细节：">样式微调</span>
      <span class="mg-feedback-chip" data-text="请为当前看板初始化 5 条测试数据：">造测试数据</span>
      <span class="mg-feedback-chip" data-text="调整交互逻辑与点击行为：">交互优化</span>
    </div>
    <textarea class="mg-feedback-textarea" placeholder="描述你看到的问题或代码调整需求，AI 将在当前分支直接修改并热更…"></textarea>
    <button class="mg-feedback-submit">提交给 Agent 实时修改</button>
    <div class="mg-feedback-status" style="display:none;"></div>
  `;

  pillWrap.appendChild(pill);
  pillWrap.appendChild(dismissBtn);
  root.appendChild(pillWrap);
  root.appendChild(drawer);
  document.body.appendChild(root);

  var textarea = drawer.querySelector('.mg-feedback-textarea');
  var submitBtn = drawer.querySelector('.mg-feedback-submit');
  var closeBtn = drawer.querySelector('.mg-feedback-close');
  var statusDiv = drawer.querySelector('.mg-feedback-status');
  var chips = drawer.querySelectorAll('.mg-feedback-chip');

  // Toggle entire widget visibility (pill + drawer) for clean screenshots/recording
  function toggleWidgetVisibility() {
    var isVisible = root.style.display !== 'none';
    if (isVisible) {
      root.style.display = 'none';
      drawer.style.display = 'none';
    } else {
      root.style.display = 'block';
    }
  }

  // Toggle drawer logic when pill is clicked
  function toggleDrawer(forceOpen) {
    var isOpen = drawer.style.display === 'flex';
    var next = forceOpen !== undefined ? forceOpen : !isOpen;
    drawer.style.display = next ? 'flex' : 'none';
    if (next) {
      setTimeout(function () { textarea.focus(); }, 50);
    }
  }

  // Draggable logic
  var isDragging = false;
  var dragStartX = 0;
  var dragStartY = 0;
  var initialLeft = 0;
  var initialTop = 0;
  var hasMoved = false;

  function onPointerDown(e) {
    if (e.target === dismissBtn || e.target.closest('.mg-feedback-dismiss-btn')) return;
    isDragging = true;
    hasMoved = false;
    var clientX = e.touches ? e.touches[0].clientX : e.clientX;
    var clientY = e.touches ? e.touches[0].clientY : e.clientY;
    dragStartX = clientX;
    dragStartY = clientY;

    var rect = root.getBoundingClientRect();
    initialLeft = rect.left;
    initialTop = rect.top;

    window.addEventListener('mousemove', onPointerMove, { passive: false });
    window.addEventListener('touchmove', onPointerMove, { passive: false });
    window.addEventListener('mouseup', onPointerUp);
    window.addEventListener('touchend', onPointerUp);
  }

  function onPointerMove(e) {
    if (!isDragging) return;
    var clientX = e.touches ? e.touches[0].clientX : e.clientX;
    var clientY = e.touches ? e.touches[0].clientY : e.clientY;
    var dx = clientX - dragStartX;
    var dy = clientY - dragStartY;

    if (Math.abs(dx) > 3 || Math.abs(dy) > 3) {
      hasMoved = true;
      e.preventDefault();
      var nextX = Math.max(10, Math.min(window.innerWidth - root.offsetWidth - 10, initialLeft + dx));
      var nextY = Math.max(10, Math.min(window.innerHeight - root.offsetHeight - 10, initialTop + dy));

      root.style.bottom = 'auto';
      root.style.right = 'auto';
      root.style.left = nextX + 'px';
      root.style.top = nextY + 'px';

      // Smart drawer placement (above or below based on top position)
      if (nextY > window.innerHeight / 2) {
        drawer.style.bottom = '44px';
        drawer.style.top = 'auto';
      } else {
        drawer.style.top = '44px';
        drawer.style.bottom = 'auto';
      }
    }
  }

  function onPointerUp(e) {
    if (!isDragging) return;
    isDragging = false;
    window.removeEventListener('mousemove', onPointerMove);
    window.removeEventListener('touchmove', onPointerMove);
    window.removeEventListener('mouseup', onPointerUp);
    window.removeEventListener('touchend', onPointerUp);

    if (!hasMoved) {
      toggleDrawer();
    }
  }

  pill.addEventListener('mousedown', onPointerDown);
  pill.addEventListener('touchstart', onPointerDown, { passive: true });

  // Dismiss pill completely
  dismissBtn.addEventListener('click', function (e) {
    e.stopPropagation();
    root.style.display = 'none';
    drawer.style.display = 'none';
  });

  closeBtn.addEventListener('click', function () {
    drawer.style.display = 'none';
  });

  // Chip click handler
  chips.forEach(function (chip) {
    chip.addEventListener('click', function () {
      var text = chip.getAttribute('data-text');
      textarea.value = text + ' ' + textarea.value;
      textarea.focus();
    });
  });

  // Submit feedback
  submitBtn.addEventListener('click', function () {
    var content = textarea.value.trim();
    if (!content) return;

    submitBtn.disabled = true;
    submitBtn.textContent = '正在发送给 Agent…';
    statusDiv.style.display = 'none';

    var apiURL = '/api/v1/projects/' + encodeURIComponent(project || 'current') + '/tasks/' + encodeURIComponent(taskId) + '/preview/feedback';

    fetch(apiURL, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ feedback: content })
    })
      .then(function (res) { return res.json(); })
      .then(function (data) {
        submitBtn.disabled = false;
        submitBtn.textContent = '提交给 Agent 实时修改';
        if (data.ok) {
          statusDiv.textContent = '✓ 意见已转交给 Agent，正在当前分支重构并热更！';
          statusDiv.style.display = 'block';
          textarea.value = '';
          setTimeout(function () {
            drawer.style.display = 'none';
            statusDiv.style.display = 'none';
          }, 2400);
        } else {
          statusDiv.textContent = '提交失败: ' + (data.message || '未知错误');
          statusDiv.style.color = '#ef4444';
          statusDiv.style.display = 'block';
        }
      })
      .catch(function (err) {
        submitBtn.disabled = false;
        submitBtn.textContent = '提交给 Agent 实时修改';
        statusDiv.textContent = '网络错误: ' + err.message;
        statusDiv.style.color = '#ef4444';
        statusDiv.style.display = 'block';
      });
  });

  // Global Keyboard Shortcuts: Cmd+K / Ctrl+K toggles whole assistant button; Esc closes drawer
  window.addEventListener('keydown', function (e) {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
      e.preventDefault();
      toggleWidgetVisibility();
    } else if (e.key === 'Escape') {
      if (drawer.style.display === 'flex') {
        drawer.style.display = 'none';
      }
    }
  });
})();
