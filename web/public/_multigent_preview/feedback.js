/**
 * Multigent In-Context Multi-Turn Assistant & Visual DOM Copilot Widget
 * Injected automatically into ephemeral preview environments.
 */
(function () {
  if (window.__MULTIGENT_FEEDBACK_INITIALIZED__) return;
  window.__MULTIGENT_FEEDBACK_INITIALIZED__ = true;

  // Extract metadata
  var currentScript = document.currentScript || document.querySelector('script[data-task-id]');
  var taskId = (currentScript && currentScript.getAttribute('data-task-id')) || window.__MG_PREVIEW_TASK_ID__ || '';
  var project = (currentScript && currentScript.getAttribute('data-project')) || window.__MG_PREVIEW_PROJECT__ || '';

  if (!taskId) {
    var match = window.location.pathname.match(/\/preview\/([^\/]+)/);
    if (match) taskId = match[1];
  }
  if (!taskId) {
    try { taskId = sessionStorage.getItem('__mg_preview_task_id') || ''; } catch(e) {}
  }
  if (!project) {
    try { project = sessionStorage.getItem('__mg_preview_project') || ''; } catch(e) {}
  }

  var isMac = /Mac|iPod|iPhone|iPad/.test(navigator.platform);
  var kbdText = isMac ? '⌘K' : 'Ctrl+K';
  var sendKbdText = isMac ? '⌘+Enter' : 'Ctrl+Enter';
  var STORAGE_CHAT_KEY = 'mg-preview-chat-' + (taskId || 'default');
  var STORAGE_POS_KEY = 'mg-preview-btn-pos';

  // Inject CSS styles
  var style = document.createElement('style');
  style.textContent = `
    .mg-copilot-root {
      position: fixed;
      bottom: 24px;
      right: 24px;
      z-index: 2147483647;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      user-select: none;
      -webkit-user-select: none;
      pointer-events: auto;
      white-space: normal;
      width: max-content;
      height: auto;
      box-sizing: border-box;
    }
    .mg-copilot-pill-wrap {
      position: relative;
      display: inline-flex;
      align-items: center;
      white-space: nowrap;
      flex-shrink: 0;
    }
    .mg-copilot-pill {
      display: inline-flex;
      align-items: center;
      gap: 7px;
      padding: 7px 14px;
      background: rgba(15, 23, 42, 0.90);
      color: #f8fafc;
      border: 1px solid rgba(255, 255, 255, 0.16);
      border-radius: 9999px;
      box-shadow: 0 10px 25px -4px rgba(0, 0, 0, 0.3), 0 0 0 1px rgba(255, 255, 255, 0.06);
      font-size: 12.5px;
      font-weight: 500;
      cursor: grab;
      backdrop-filter: blur(14px);
      -webkit-backdrop-filter: blur(14px);
      transition: background 0.15s ease, box-shadow 0.15s ease, transform 0.15s ease;
      touch-action: none;
      white-space: nowrap;
      flex-shrink: 0;
      box-sizing: border-box;
    }
    .mg-copilot-pill:hover {
      background: rgba(15, 23, 42, 0.98);
      box-shadow: 0 14px 30px -4px rgba(0, 0, 0, 0.4);
      transform: translateY(-1px);
    }
    .mg-copilot-pill:active {
      cursor: grabbing;
      transform: scale(0.98);
    }
    .mg-copilot-dot {
      width: 7px;
      height: 7px;
      border-radius: 50%;
      background: #10b981;
      box-shadow: 0 0 8px #10b981;
      flex-shrink: 0;
      transition: background 0.2s ease, box-shadow 0.2s ease;
    }
    .mg-copilot-dot.busy {
      background: #f59e0b;
      box-shadow: 0 0 8px #f59e0b;
      animation: mgGlow 1.5s infinite ease-in-out;
    }
    @keyframes mgGlow {
      0%, 100% { opacity: 1; box-shadow: 0 0 4px #f59e0b; }
      50% { opacity: 0.5; box-shadow: 0 0 10px #f59e0b; }
    }
    .mg-copilot-kbd-hint {
      font-size: 10px;
      color: #94a3b8;
      background: rgba(255, 255, 255, 0.12);
      border-radius: 4px;
      padding: 1px 4px;
      margin-left: 2px;
      font-family: ui-monospace, monospace;
    }
    .mg-copilot-dismiss-btn {
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
    .mg-copilot-pill-wrap:hover .mg-copilot-dismiss-btn {
      display: flex;
    }
    .mg-copilot-dismiss-btn:hover {
      background: #ef4444;
    }
    .mg-copilot-drawer {
      position: absolute;
      bottom: 44px;
      right: 0;
      width: 440px;
      height: 540px;
      max-width: calc(100vw - 32px);
      max-height: calc(100vh - 80px);
      background: #ffffff;
      border: 1px solid #e2e8f0;
      border-radius: 16px;
      box-shadow: 0 24px 48px -12px rgba(15, 23, 42, 0.22), 0 0 0 1px rgba(15, 23, 42, 0.06);
      display: none;
      flex-direction: column;
      overflow: hidden;
      z-index: 10;
      white-space: normal;
      box-sizing: border-box;
      animation: mgDrawerIn 0.22s cubic-bezier(0.16, 1, 0.3, 1);
    }
    .mg-copilot-drawer * {
      box-sizing: border-box;
    }
    @keyframes mgDrawerIn {
      from { opacity: 0; transform: translateY(10px) scale(0.97); }
      to { opacity: 1; transform: translateY(0) scale(1); }
    }
    .mg-copilot-header {
      padding: 11px 16px;
      border-bottom: 1px solid #f1f5f9;
      display: flex;
      align-items: center;
      justify-content: space-between;
      background: #fafafa;
    }
    .mg-copilot-header-left {
      display: flex;
      align-items: center;
      gap: 8px;
    }
    .mg-copilot-header-title {
      font-size: 13.5px;
      font-weight: 700;
      color: #0f172a;
    }
    .mg-copilot-status-badge {
      font-size: 10.5px;
      padding: 2px 7px;
      border-radius: 9999px;
      background: #ecfdf5;
      color: #059669;
      border: 1px solid #a7f3d0;
      font-weight: 500;
    }
    .mg-copilot-status-badge.busy {
      position: relative;
      overflow: hidden;
      background: #fffbeb;
      color: #b45309;
      border-color: #fde68a;
    }
    .mg-copilot-status-badge.busy::after {
      content: '';
      position: absolute;
      top: -50%;
      left: -100%;
      width: 80%;
      height: 200%;
      background: linear-gradient(
        90deg,
        transparent 0%,
        rgba(255, 255, 255, 0.8) 50%,
        transparent 100%
      );
      transform: rotate(25deg);
      animation: mgShimmer 2.2s infinite ease-in-out;
    }
    .mg-copilot-header-actions {
      display: flex;
      align-items: center;
      gap: 5px;
    }
    .mg-copilot-btn-action {
      background: #ffffff;
      border: 1px solid #e2e8f0;
      color: #334155;
      cursor: pointer;
      font-size: 11px;
      font-weight: 500;
      padding: 3px 8px;
      border-radius: 6px;
      display: inline-flex;
      align-items: center;
      gap: 4px;
      transition: all 0.15s ease;
    }
    .mg-copilot-btn-action:hover {
      background: #f1f5f9;
      color: #0284c7;
      border-color: #cbd5e1;
    }
    .mg-copilot-btn-action.btn-inspect {
      color: #0f172a;
      background: #ffffff;
      border: 1px solid #cbd5e1;
      font-weight: 600;
      box-shadow: 0 1px 2px rgba(0, 0, 0, 0.05);
    }
    .mg-copilot-btn-action.btn-inspect:hover {
      background: #f8fafc;
      color: #0284c7;
      border-color: #0284c7;
      box-shadow: 0 2px 8px rgba(2, 132, 199, 0.15);
      transform: translateY(-0.5px);
    }
    .mg-copilot-btn-action.btn-inspect svg {
      color: #0f172a;
      transition: color 0.15s;
    }
    .mg-copilot-btn-action.btn-inspect:hover svg {
      color: #0284c7;
    }
    .mg-copilot-btn-close {
      background: none;
      border: none;
      color: #94a3b8;
      cursor: pointer;
      font-size: 14px;
      padding: 2px 6px;
      border-radius: 6px;
      line-height: 1;
      transition: background 0.15s, color 0.15s;
    }
    .mg-copilot-btn-close:hover {
      background: #f1f5f9;
      color: #334155;
    }
    .mg-copilot-body {
      flex: 1;
      overflow-y: auto;
      overflow-x: hidden;
      padding: 14px;
      display: flex;
      flex-direction: column;
      gap: 12px;
      background: #ffffff;
      scroll-behavior: smooth;
      white-space: normal;
      box-sizing: border-box;
    }
    .mg-copilot-empty {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      height: 100%;
      color: #64748b;
      text-align: center;
      gap: 10px;
      padding: 20px 10px;
      box-sizing: border-box;
    }
    .mg-copilot-empty-title {
      font-size: 13.5px;
      font-weight: 600;
      color: #334155;
    }
    .mg-copilot-empty-desc {
      font-size: 12px;
      color: #94a3b8;
      line-height: 1.5;
      white-space: normal;
      word-break: break-word;
      max-width: 100%;
    }
    .mg-copilot-chips {
      display: flex;
      flex-wrap: wrap;
      gap: 6px;
      justify-content: center;
      margin-top: 6px;
      white-space: normal;
      max-width: 100%;
    }
    .mg-copilot-chip {
      padding: 4px 10px;
      background: #f8fafc;
      border: 1px solid #e2e8f0;
      border-radius: 9999px;
      font-size: 11.5px;
      color: #475569;
      cursor: pointer;
      white-space: nowrap;
      transition: all 0.15s ease;
    }
    .mg-copilot-chip:hover {
      background: #e0f2fe;
      color: #0284c7;
      border-color: #bae6fd;
    }
    .mg-msg-user {
      align-self: flex-end;
      max-width: 88%;
      background: #0284c7;
      color: #ffffff;
      padding: 9px 13px;
      border-radius: 16px 16px 4px 16px;
      font-size: 12.5px;
      line-height: 1.5;
      word-break: break-word;
      box-shadow: 0 3px 10px rgba(2, 132, 199, 0.22);
    }
    .mg-user-dom-pill {
      display: inline-flex;
      align-items: center;
      gap: 4px;
      background: rgba(255, 255, 255, 0.22);
      border: 1px solid rgba(255, 255, 255, 0.4);
      border-radius: 6px;
      padding: 2px 7px;
      font-family: ui-monospace, monospace;
      font-size: 11px;
      font-weight: 600;
      margin-bottom: 5px;
      width: fit-content;
    }
    .mg-msg-assistant {
      align-self: flex-start;
      width: 100%;
      max-width: 100%;
      background: #ffffff;
      border: 1px solid #e2e8f0;
      color: #1e293b;
      padding: 12px 14px;
      border-radius: 16px 16px 16px 4px;
      font-size: 12.5px;
      line-height: 1.55;
      word-break: break-word;
      display: flex;
      flex-direction: column;
      gap: 10px;
      box-shadow: 0 2px 8px rgba(15, 23, 42, 0.04);
      box-sizing: border-box;
    }
    .mg-thinking-indicator {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      color: #0284c7;
      font-size: 12px;
      font-weight: 500;
    }
    .mg-thinking-spinner {
      width: 12px;
      height: 12px;
      border: 2px solid #bae6fd;
      border-top-color: #0284c7;
      border-radius: 50%;
      animation: mgSpin 0.8s linear infinite;
    }
    @keyframes mgSpin {
      to { transform: rotate(360deg); }
    }
    /* Collapsible Tool Timeline (like ConversationLog) */
    .mg-tool-group {
      border: 1px solid #e2e8f0;
      background: #f8fafc;
      border-radius: 10px;
      overflow: hidden;
      transition: all 0.15s ease;
    }
    .mg-tool-group-summary {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 6px 10px;
      cursor: pointer;
      font-size: 11.5px;
      font-weight: 500;
      color: #475569;
      background: #f1f5f9;
      user-select: none;
      list-style: none;
    }
    .mg-tool-group-summary::-webkit-details-marker {
      display: none;
    }
    .mg-tool-summary-left {
      display: flex;
      align-items: center;
      gap: 6px;
    }
    .mg-tool-live-badge {
      position: relative;
      overflow: hidden;
      font-size: 10px;
      padding: 1.5px 7px;
      border-radius: 9999px;
      background: #fef3c7;
      color: #92400e;
      border: 1px solid #fde68a;
      font-weight: 600;
      display: inline-flex;
      align-items: center;
      gap: 3px;
    }
    .mg-tool-live-badge::after {
      content: '';
      position: absolute;
      top: -50%;
      left: -100%;
      width: 80%;
      height: 200%;
      background: linear-gradient(
        90deg,
        transparent 0%,
        rgba(255, 255, 255, 0.85) 50%,
        transparent 100%
      );
      transform: rotate(25deg);
      animation: mgShimmer 1.8s infinite ease-in-out;
    }
    @keyframes mgShimmer {
      0% { left: -100%; }
      55%, 100% { left: 160%; }
    }
    .mg-tool-summary-toggle {
      font-size: 11px;
      color: #0284c7;
      font-weight: 500;
    }
    .mg-tool-steps-list {
      padding: 6px 8px;
      display: flex;
      flex-direction: column;
      gap: 4px;
      max-height: 160px;
      overflow-y: auto;
      background: #ffffff;
      border-top: 1px solid #e2e8f0;
    }
    .mg-tool-step-item {
      display: flex;
      align-items: center;
      gap: 6px;
      padding: 3px 6px;
      border-radius: 6px;
      background: #f8fafc;
      font-family: ui-monospace, monospace;
      font-size: 11px;
    }
    .mg-tool-step-badge {
      padding: 1px 5px;
      border-radius: 4px;
      font-size: 10px;
      font-weight: 600;
      white-space: nowrap;
      flex-shrink: 0;
    }
    .mg-tool-step-badge.bash { background: #e0e7ff; color: #4338ca; }
    .mg-tool-step-badge.read { background: #e0f2fe; color: #0369a1; }
    .mg-tool-step-badge.edit { background: #fef3c7; color: #b45309; }
    .mg-tool-step-badge.search { background: #ede9fe; color: #6d28d9; }
    .mg-tool-step-badge.other { background: #e2e8f0; color: #334155; }
    .mg-tool-step-target {
      color: #334155;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      flex: 1;
    }
    /* Markdown Styles */
    .mg-md-wrap {
      color: #1e293b;
      font-size: 12.5px;
      line-height: 1.55;
      word-break: break-word;
    }
    .mg-md-h1, .mg-md-h2, .mg-md-h3 {
      margin: 8px 0 4px;
      font-weight: 700;
      color: #0f172a;
    }
    .mg-md-h1 { font-size: 14px; }
    .mg-md-h2 { font-size: 13.5px; }
    .mg-md-h3 { font-size: 13px; }
    .mg-md-inline-code {
      background: #f1f5f9;
      color: #0369a1;
      border: 1px solid #e2e8f0;
      border-radius: 4px;
      padding: 1px 5px;
      font-family: ui-monospace, monospace;
      font-size: 11px;
    }
    .mg-md-code-box {
      position: relative;
      background: #0f172a;
      border-radius: 8px;
      margin: 8px 0;
      overflow: hidden;
      box-shadow: 0 4px 14px rgba(0,0,0,0.15);
    }
    .mg-md-code-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 4px 10px;
      background: #1e293b;
      font-size: 10px;
      color: #94a3b8;
      font-family: ui-monospace, monospace;
      text-transform: uppercase;
    }
    .mg-md-code {
      margin: 0;
      padding: 8px 10px;
      overflow-x: auto;
      color: #f8fafc;
      font-family: ui-monospace, monospace;
      font-size: 11px;
      line-height: 1.45;
    }
    .mg-diff-add {
      background: rgba(16, 185, 129, 0.2);
      color: #6ee7b7;
      display: block;
      padding: 0 4px;
      border-radius: 2px;
    }
    .mg-diff-del {
      background: rgba(239, 68, 68, 0.2);
      color: #fca5a5;
      display: block;
      padding: 0 4px;
      border-radius: 2px;
    }
    .mg-md-li {
      display: flex;
      gap: 6px;
      margin-bottom: 3px;
      line-height: 1.45;
    }
    .mg-md-bullet {
      color: #0284c7;
      font-weight: bold;
    }
    .mg-md-quote {
      border-left: 3px solid #0284c7;
      padding-left: 8px;
      margin: 6px 0;
      color: #475569;
      font-style: italic;
      background: #f8fafc;
      border-radius: 0 4px 4px 0;
    }
    .mg-md-p-gap {
      height: 6px;
    }
    .mg-completion-banner {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 8px;
      background: #f0fdf4;
      border: 1px solid #bbf7d0;
      border-radius: 8px;
      padding: 8px 10px;
      color: #166534;
      font-size: 11.5px;
      margin-top: 4px;
    }
    .mg-completion-left {
      display: flex;
      align-items: center;
      gap: 6px;
    }
    .mg-refresh-btn {
      background: #16a34a;
      color: #ffffff;
      border: none;
      border-radius: 5px;
      padding: 3px 8px;
      font-size: 11px;
      font-weight: 600;
      cursor: pointer;
      transition: background 0.15s;
    }
    .mg-refresh-btn:hover {
      background: #15803d;
    }
    .mg-stopped-banner {
      display: inline-flex;
      align-items: center;
      gap: 5px;
      background: #fef2f2;
      border: 1px solid #fecaca;
      border-radius: 6px;
      padding: 4px 8px;
      color: #b91c1c;
      font-size: 11.5px;
      font-weight: 500;
    }
    .mg-dom-tag-badge {
      position: relative;
      display: inline-flex;
      align-items: center;
      gap: 6px;
      padding: 3px 8px;
      background: #f0f9ff;
      border: 1px solid #bae6fd;
      border-radius: 7px;
      color: #0284c7;
      font-size: 11.5px;
      font-weight: 500;
      margin-bottom: 5px;
      cursor: pointer;
      transition: all 0.15s ease;
      width: fit-content;
    }
    .mg-dom-tag-badge:hover {
      background: #e0f2fe;
      border-color: #7dd3fc;
    }
    .mg-dom-tag-left {
      display: inline-flex;
      align-items: center;
      gap: 4px;
      font-family: ui-monospace, monospace;
    }
    .mg-dom-tag-label {
      font-weight: 700;
      color: #0284c7;
    }
    .mg-dom-tag-name {
      color: #0369a1;
      font-size: 11px;
      max-width: 140px;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .mg-dom-hover-card {
      position: absolute;
      bottom: calc(100% + 8px);
      left: 0;
      width: 320px;
      max-width: calc(100vw - 64px);
      background: #0f172a;
      color: #f8fafc;
      border-radius: 10px;
      padding: 10px 12px;
      box-shadow: 0 16px 36px -4px rgba(0, 0, 0, 0.4), 0 0 0 1px rgba(255, 255, 255, 0.1);
      display: none;
      flex-direction: column;
      gap: 6px;
      z-index: 100;
      pointer-events: none;
      font-family: -apple-system, BlinkMacSystemFont, sans-serif;
      animation: mgCardPop 0.15s ease;
    }
    .mg-dom-tag-badge:hover .mg-dom-hover-card {
      display: flex;
    }
    .mg-dom-card-header {
      font-size: 11px;
      font-weight: 700;
      color: #38bdf8;
      border-bottom: 1px solid rgba(255, 255, 255, 0.1);
      padding-bottom: 4px;
    }
    .mg-dom-card-row {
      display: flex;
      flex-direction: column;
      gap: 2px;
      font-size: 10.5px;
      line-height: 1.4;
    }
    .mg-dom-card-k {
      color: #94a3b8;
      font-weight: 600;
    }
    .mg-dom-card-v {
      color: #f1f5f9;
    }
    .mg-dom-card-code {
      background: rgba(255, 255, 255, 0.1);
      padding: 2px 5px;
      border-radius: 4px;
      font-family: ui-monospace, monospace;
      color: #7dd3fc;
      word-break: break-all;
    }
    .mg-dom-card-pre {
      margin: 0;
      padding: 4px 6px;
      background: rgba(0, 0, 0, 0.4);
      border-radius: 4px;
      color: #cbd5e1;
      font-family: ui-monospace, monospace;
      font-size: 10px;
      overflow-x: auto;
      white-space: pre-wrap;
      word-break: break-all;
      max-height: 80px;
    }
    @keyframes mgCardPop {
      from { opacity: 0; transform: translateY(4px) scale(0.98); }
      to { opacity: 1; transform: translateY(0) scale(1); }
    }
    .mg-dom-tag-remove {
      cursor: pointer;
      color: #0284c7;
      font-size: 14px;
      line-height: 1;
      margin-left: 2px;
    }
    .mg-dom-tag-remove:hover {
      color: #ef4444;
    }
    .mg-copilot-footer {
      padding: 10px 14px 12px;
      border-top: 1px solid #f1f5f9;
      background: #fafafa;
      display: flex;
      flex-direction: column;
      gap: 6px;
    }
    .mg-copilot-input-box {
      display: flex;
      gap: 8px;
      align-items: flex-end;
    }
    .mg-copilot-textarea {
      flex: 1;
      height: 52px;
      min-height: 52px;
      max-height: 120px;
      box-sizing: border-box;
      border: 1px solid #cbd5e1;
      border-radius: 10px;
      padding: 8px 10px;
      font-size: 12px;
      color: #1e293b;
      outline: none;
      resize: none;
      font-family: inherit;
      background: #ffffff;
      line-height: 1.42;
      overflow-y: hidden;
      scrollbar-width: none;
      -ms-overflow-style: none;
    }
    .mg-copilot-textarea::-webkit-scrollbar {
      display: none;
    }
    .mg-copilot-textarea:focus {
      border-color: #0284c7;
      box-shadow: 0 0 0 3px rgba(2, 132, 199, 0.12);
    }
    .mg-copilot-submit-btn {
      padding: 8px 15px;
      background: #0284c7;
      color: #ffffff;
      border: none;
      border-radius: 10px;
      font-size: 12.5px;
      font-weight: 600;
      cursor: pointer;
      height: 52px;
      min-height: 52px;
      display: flex;
      align-items: center;
      justify-content: center;
      transition: background 0.15s ease, transform 0.15s ease;
      white-space: nowrap;
    }
    .mg-copilot-submit-btn:hover {
      background: #0369a1;
    }
    .mg-copilot-submit-btn.stop {
      background: #ef4444;
      box-shadow: 0 4px 12px rgba(239, 68, 68, 0.3);
    }
    .mg-copilot-submit-btn.stop:hover {
      background: #dc2626;
    }
    .mg-copilot-submit-btn:disabled {
      opacity: 0.6;
      cursor: not-allowed;
    }
    .mg-copilot-tips {
      font-size: 11px;
      color: #94a3b8;
      display: flex;
      align-items: center;
      justify-content: space-between;
    }
    /* DOM Inspector Styles */
    .mg-inspector-overlay {
      position: fixed;
      pointer-events: none;
      border: 2px solid #0284c7;
      background: rgba(2, 132, 199, 0.16);
      border-radius: 4px;
      z-index: 2147483647;
      margin: 0;
      padding: 0;
      display: none;
      transition: all 0.05s ease-out;
      box-shadow: 0 0 0 1px rgba(255, 255, 255, 0.6);
    }
    .mg-inspector-badge {
      position: absolute;
      top: -22px;
      left: 0;
      background: #0284c7;
      color: #ffffff;
      font-size: 10px;
      font-family: ui-monospace, monospace;
      padding: 1px 6px;
      border-radius: 3px;
      white-space: nowrap;
      pointer-events: none;
      box-shadow: 0 2px 6px rgba(0, 0, 0, 0.2);
    }
    .mg-inspector-bar {
      position: fixed;
      top: 16px;
      left: 50%;
      transform: translateX(-50%);
      z-index: 2147483647;
      background: rgba(15, 23, 42, 0.92);
      color: #ffffff;
      padding: 7px 16px;
      border-radius: 9999px;
      border: none;
      margin: 0;
      box-shadow: 0 12px 30px rgba(0, 0, 0, 0.35), 0 0 0 1px rgba(255, 255, 255, 0.15);
      backdrop-filter: blur(12px);
      -webkit-backdrop-filter: blur(12px);
      display: none;
      align-items: center;
      gap: 12px;
      font-size: 12.5px;
      font-weight: 500;
      animation: mgBarIn 0.2s cubic-bezier(0.16, 1, 0.3, 1);
    }
    @keyframes mgBarIn {
      from { opacity: 0; transform: translate(-50%, -10px); }
      to { opacity: 1; transform: translate(-50%, 0); }
    }
    .mg-inspector-bar-cancel {
      background: rgba(255, 255, 255, 0.16);
      border: 1px solid rgba(255, 255, 255, 0.25);
      color: #f8fafc;
      padding: 2px 8px;
      border-radius: 6px;
      font-size: 11px;
      cursor: pointer;
      transition: background 0.15s;
    }
    .mg-inspector-bar-cancel:hover {
      background: rgba(255, 255, 255, 0.28);
    }
  `;
  document.head.appendChild(style);

  // Build UI
  var root = document.createElement('div');
  root.className = 'mg-copilot-root';

  var pillWrap = document.createElement('div');
  pillWrap.className = 'mg-copilot-pill-wrap';

  var pill = document.createElement('div');
  pill.className = 'mg-copilot-pill';
  pill.innerHTML = '<span class="mg-copilot-dot"></span><span class="mg-copilot-text">智能助手</span><span class="mg-copilot-kbd-hint">' + kbdText + '</span>';

  var dismissBtn = document.createElement('button');
  dismissBtn.className = 'mg-copilot-dismiss-btn';
  dismissBtn.innerHTML = '&times;';
  dismissBtn.title = '隐藏悬浮球 (' + kbdText + ' 重新呼出)';

  var drawer = document.createElement('div');
  drawer.className = 'mg-copilot-drawer';
  drawer.innerHTML = `
    <div class="mg-copilot-header">
      <div class="mg-copilot-header-left">
        <span class="mg-copilot-header-title">边看边改</span>
        <span class="mg-copilot-status-badge">就绪</span>
      </div>
      <div class="mg-copilot-header-actions">
        <button class="mg-copilot-btn-action btn-inspect" title="在页面上框选需要修改的目标元素 (Esc 取消)">
          <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.3" stroke-linecap="round" stroke-linejoin="round">
            <circle cx="12" cy="12" r="10"/>
            <circle cx="12" cy="12" r="3"/>
            <line x1="12" y1="2" x2="12" y2="6"/>
            <line x1="12" y1="18" x2="12" y2="22"/>
            <line x1="2" y1="12" x2="6" y2="12"/>
            <line x1="18" y1="12" x2="22" y2="12"/>
          </svg>
          <span>选元素</span>
        </button>
        <button class="mg-copilot-btn-action btn-clear" title="清空会话记录">清空</button>
        <button class="mg-copilot-btn-close" title="关闭窗口">&times;</button>
      </div>
    </div>
    <div class="mg-copilot-body">
      <div class="mg-copilot-empty">
        <div class="mg-copilot-empty-title">在当前分支直接修改代码</div>
        <div class="mg-copilot-empty-desc">针对当前预览界面提出任何视觉或逻辑微调，Agent 将直接在专属 Worktree 内完成修改并即时热重载。</div>
        <div class="mg-copilot-chips">
          <span class="mg-copilot-chip" data-text="修复页面样式与排版布局：">🎨 样式微调</span>
          <span class="mg-copilot-chip" data-text="为当前页面造 5 条逼真的演示测试数据：">📊 造测试数据</span>
          <span class="mg-copilot-chip" data-text="优化交互逻辑与点击行为：">✨ 交互优化</span>
          <span class="mg-copilot-chip" data-text="适配暗色模式深色主题：">🌙 深色适配</span>
        </div>
      </div>
    </div>
    <div class="mg-copilot-footer">
      <div class="mg-dom-tag-container" style="display:none;"></div>
      <div class="mg-copilot-input-box">
        <textarea class="mg-copilot-textarea" placeholder="描述你看到的问题或代码改动需求… (点击发送按钮 或 ${sendKbdText} 发送)"></textarea>
        <button class="mg-copilot-submit-btn">发送</button>
      </div>
      <div class="mg-copilot-tips">
        <span>快捷键 ${kbdText} 随时无痕隐藏/唤出</span>
        <span class="mg-copilot-mode-hint">Worktree 独立沙箱</span>
      </div>
    </div>
  `;

  pillWrap.appendChild(pill);
  pillWrap.appendChild(dismissBtn);
  root.appendChild(pillWrap);
  root.appendChild(drawer);
  document.body.appendChild(root);

  // Inspector Elements
  var inspectorOverlay = document.createElement('div');
  inspectorOverlay.className = 'mg-inspector-overlay';
  var inspectorBadge = document.createElement('div');
  inspectorBadge.className = 'mg-inspector-badge';
  inspectorOverlay.appendChild(inspectorBadge);
  document.body.appendChild(inspectorOverlay);

  var inspectorBar = document.createElement('div');
  inspectorBar.className = 'mg-inspector-bar';
  inspectorBar.innerHTML = '<span>🎯 请在页面上点击需要修改的目标元素</span><button class="mg-inspector-bar-cancel">取消 (Esc)</button>';
  document.body.appendChild(inspectorBar);

  // Initialize position from localStorage
  var savedPos = null;
  try { savedPos = JSON.parse(localStorage.getItem(STORAGE_POS_KEY)); } catch(e) {}
  if (savedPos && typeof savedPos.left === 'number' && typeof savedPos.top === 'number') {
    var winW = window.innerWidth || document.documentElement.clientWidth || 1000;
    var winH = window.innerHeight || document.documentElement.clientHeight || 800;
    var curX = Math.max(10, Math.min(winW - 130, savedPos.left));
    var curY = Math.max(10, Math.min(winH - 50, savedPos.top));
    root.style.left = curX + 'px';
    root.style.top = curY + 'px';
    root.style.right = 'auto';
    root.style.bottom = 'auto';
    if (curX < 460) {
      drawer.style.left = '0px';
      drawer.style.right = 'auto';
    }
    if (curY <= winH / 2) {
      drawer.style.top = '44px';
      drawer.style.bottom = 'auto';
    }
  }

  var statusBadge = drawer.querySelector('.mg-copilot-status-badge');
  var dot = pill.querySelector('.mg-copilot-dot');
  var bodyContainer = drawer.querySelector('.mg-copilot-body');
  var textarea = drawer.querySelector('.mg-copilot-textarea');
  var submitBtn = drawer.querySelector('.mg-copilot-submit-btn');
  var clearBtn = drawer.querySelector('.btn-clear');
  var inspectBtn = drawer.querySelector('.btn-inspect');
  var closeBtn = drawer.querySelector('.mg-copilot-btn-close');
  var emptyState = drawer.querySelector('.mg-copilot-empty');
  var chips = drawer.querySelectorAll('.mg-copilot-chip');
  var domTagContainer = drawer.querySelector('.mg-dom-tag-container');

  // Load chat history from localStorage
  var msgs = [];
  try {
    var saved = localStorage.getItem(STORAGE_CHAT_KEY);
    if (saved) msgs = JSON.parse(saved);
  } catch(e) {}

  var isStreaming = false;
  var abortCtrl = null;
  var isInspectorActive = false;
  var selectedDOMTarget = null;

  function saveMessages() {
    try {
      localStorage.setItem(STORAGE_CHAT_KEY, JSON.stringify(msgs));
    } catch(e) {}
  }

  function renderMessages() {
    if (msgs.length === 0) {
      bodyContainer.innerHTML = '';
      bodyContainer.appendChild(emptyState);
      return;
    }
    bodyContainer.innerHTML = '';
    msgs.forEach(function (m) {
      var div = document.createElement('div');
      if (m.role === 'user') {
        div.className = 'mg-msg-user';
        div.innerHTML = renderUserBubble(m);
      } else {
        div.className = 'mg-msg-assistant';
        div.innerHTML = renderAssistantBubble(m);
      }
      bodyContainer.appendChild(div);
    });
    bodyContainer.scrollTop = bodyContainer.scrollHeight;
  }

  function renderUserBubble(m) {
    var raw = m.content || '';
    var domMatch = raw.match(/^(@<[^>]+>)\s*([\s\S]*)$/);
    if (domMatch) {
      return `
        <div class="mg-user-dom-pill">🎯 ${escapeHtml(domMatch[1].slice(1))}</div>
        <div class="mg-user-text">${escapeHtml(domMatch[2])}</div>
      `;
    }
    return `<div class="mg-user-text">${escapeHtml(raw)}</div>`;
  }

  function renderMarkdown(md) {
    if (!md) return '';
    var text = escapeHtml(md);

    // 1. Fenced Code Blocks ```lang ... ```
    text = text.replace(/```([a-zA-Z0-9_\-\+]*)\n([\s\S]*?)```/g, function (m, lang, code) {
      var langLabel = lang || 'code';
      var lines = code.trim().split('\n').map(function (l) {
        if (lang === 'diff' || (!lang && (l.startsWith('+') || l.startsWith('-')))) {
          if (l.startsWith('+') && !l.startsWith('+++')) return '<div class="mg-diff-add">' + l + '</div>';
          if (l.startsWith('-') && !l.startsWith('---')) return '<div class="mg-diff-del">' + l + '</div>';
        }
        return '<div>' + (l || '&nbsp;') + '</div>';
      }).join('');
      return '<div class="mg-md-code-box"><div class="mg-md-code-header"><span>' + langLabel + '</span></div><pre class="mg-md-code"><code>' + lines + '</code></pre></div>';
    });

    // 2. Inline Code `code`
    text = text.replace(/`([^`\n]+)`/g, '<code class="mg-md-inline-code">$1</code>');

    // 3. Headers
    text = text.replace(/^### (.*$)/gim, '<div class="mg-md-h3">$1</div>');
    text = text.replace(/^## (.*$)/gim, '<div class="mg-md-h2">$1</div>');
    text = text.replace(/^# (.*$)/gim, '<div class="mg-md-h1">$1</div>');

    // 4. Bold and Italic
    text = text.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
    text = text.replace(/\*([^*]+)\*/g, '<em>$1</em>');

    // 5. Bullet Lists
    text = text.replace(/^\s*[\-\*]\s+(.*)$/gim, '<div class="mg-md-li"><span class="mg-md-bullet">•</span> <span>$1</span></div>');

    // 6. Blockquote
    text = text.replace(/^&gt;\s+(.*$)/gim, '<div class="mg-md-quote">$1</div>');

    // 7. Paragraph spacing
    text = text.replace(/\n\n+/g, '<div class="mg-md-p-gap"></div>');
    text = text.replace(/\n/g, '<br/>');

    return text;
  }

  function renderAssistantBubble(m) {
    var html = '';

    // 1. Stopped banner
    if (m.isStopped) {
      html += '<div class="mg-stopped-banner"><span>🛑 修改已被手动中止</span></div>';
    } else if (m.isThinking && !m.content && (!m.tools || m.tools.length === 0)) {
      // 2. Thinking / Active state
      html += '<div class="mg-thinking-indicator"><div class="mg-thinking-spinner"></div>正在分析代码并规划修改方案…</div>';
    }

    // 3. Structured Collapsible Tool Timeline (inspired by ConversationLog)
    if (m.tools && m.tools.length > 0) {
      var isRunning = !m.isDone && !m.isStopped;
      html += `
        <details class="mg-tool-group" ${isRunning ? 'open' : ''}>
          <summary class="mg-tool-group-summary">
            <div class="mg-tool-summary-left">
              <span class="mg-tool-summary-icon">🛠️</span>
              <span>执行了 <strong>${m.tools.length}</strong> 个操作</span>
              ${isRunning ? '<span class="mg-tool-live-badge">执行中</span>' : ''}
            </div>
            <span class="mg-tool-summary-toggle">明细 ▾</span>
          </summary>
          <div class="mg-tool-steps-list">
            ${m.tools.map(function (t) {
              var isObj = typeof t === 'object';
              var type = isObj ? t.type : (t.indexOf('Bash') !== -1 ? 'bash' : (t.indexOf('Read') !== -1 ? 'read' : (t.indexOf('Edit') !== -1 ? 'edit' : 'other')));
              var name = isObj ? t.name : (t.split(' ')[0] || 'Tool');
              var target = isObj ? t.target : (t.replace(/^[^\s]+\s*/, '') || t);
              var icon = type === 'bash' ? '⚡' : (type === 'read' ? '📖' : (type === 'edit' ? '✏️' : (type === 'search' ? '🔍' : '🔧')));
              return `
                <div class="mg-tool-step-item">
                  <span class="mg-tool-step-badge ${type}">${icon} ${escapeHtml(name)}</span>
                  <span class="mg-tool-step-target" title="${escapeHtml(target)}">${escapeHtml(target)}</span>
                </div>
              `;
            }).join('')}
          </div>
        </details>
      `;
    }

    // 4. Formatted Markdown Text output
    if (m.content) {
      html += '<div class="mg-md-wrap">' + renderMarkdown(m.content) + '</div>';
    }

    // 5. Completed notice
    if (m.isDone && !m.isStopped) {
      html += `
        <div class="mg-completion-banner">
          <div class="mg-completion-left">
            <span>✅</span>
            <span>修改已完成并触发热重载！</span>
          </div>
          <button class="mg-refresh-btn" onclick="window.location.reload()">刷新页面</button>
        </div>
      `;
    }

    if (!html) {
      html = '<div class="mg-thinking-indicator"><div class="mg-thinking-spinner"></div>正在执行修改…</div>';
    }
    return html;
  }

  function escapeHtml(s) {
    if (!s) return '';
    return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
  }

  function setBusyState(busy) {
    if (busy) {
      statusBadge.textContent = '修改中';
      statusBadge.className = 'mg-copilot-status-badge busy';
      dot.className = 'mg-copilot-dot busy';
      submitBtn.textContent = '🛑 中止';
      submitBtn.className = 'mg-copilot-submit-btn stop';
    } else {
      statusBadge.textContent = '就绪';
      statusBadge.className = 'mg-copilot-status-badge';
      dot.className = 'mg-copilot-dot';
      submitBtn.textContent = '发送';
      submitBtn.className = 'mg-copilot-submit-btn';
    }
  }

  function toggleWidgetVisibility() {
    var isVisible = root.style.display !== 'none';
    if (isVisible) {
      root.style.display = 'none';
      drawer.style.display = 'none';
    } else {
      root.style.display = 'block';
    }
  }

  function toggleDrawer(forceOpen) {
    root.style.display = 'block';
    var isOpen = drawer.style.display === 'flex';
    var next = forceOpen !== undefined ? forceOpen : !isOpen;
    drawer.style.display = next ? 'flex' : 'none';
    if (next) {
      setTimeout(function () { textarea.focus(); }, 50);
      checkStatus();
      renderMessages();
    }
  }

  function attachLiveStream(assistantMsg) {
    if (abortCtrl) abortCtrl.abort();
    abortCtrl = new AbortController();
    var liveUrl = '/api/v1/projects/' + encodeURIComponent(project || 'current') + '/tasks/' + encodeURIComponent(taskId) + '/preview/live';

    fetch(liveUrl, {
      method: 'GET',
      headers: { 'Accept': 'text/event-stream' },
      signal: abortCtrl.signal
    })
      .then(function (res) {
        if (!res.ok) return;
        var reader = res.body.getReader();
        var decoder = new TextDecoder();
        var buffer = '';

        function readChunk() {
          return reader.read().then(function (result) {
            if (result.done) {
              assistantMsg.isThinking = false;
              if (!assistantMsg.isStopped) assistantMsg.isDone = true;
              saveMessages();
              renderMessages();
              isStreaming = false;
              setBusyState(false);
              return;
            }
            buffer += decoder.decode(result.value, { stream: true });
            var parts = buffer.split('\n');
            buffer = parts.pop() || '';

            for (var i = 0; i < parts.length; i++) {
              var part = parts[i].trim();
              if (part.startsWith('data: ')) {
                var dataStr = part.slice(6);
                var isEnd = processRawEvent(dataStr, assistantMsg);
                renderMessages();
                if (isEnd) {
                  isStreaming = false;
                  setBusyState(false);
                  saveMessages();
                }
              }
            }
            return readChunk();
          });
        }
        return readChunk();
      })
      .catch(function (err) {
        if (err.name !== 'AbortError' && !window.isUnloading) {
          // Stream disconnected without abort
        }
      });
  }

  function checkStatus() {
    var statusUrl = '/api/v1/projects/' + encodeURIComponent(project || 'current') + '/tasks/' + encodeURIComponent(taskId) + '/preview/status';
    fetch(statusUrl)
      .then(function (r) { return r.json(); })
      .then(function (data) {
        if (data && data.busy) {
          setBusyState(true);
          if (!isStreaming) {
            isStreaming = true;
            var last = msgs.length > 0 ? msgs[msgs.length - 1] : null;
            var assistantMsg;
            if (last && last.role === 'assistant' && !last.isDone && !last.isStopped) {
              assistantMsg = last;
            } else {
              assistantMsg = { role: 'assistant', content: '', tools: [], isThinking: true, isDone: false, isStopped: false };
              msgs.push(assistantMsg);
            }
            renderMessages();
            attachLiveStream(assistantMsg);
          }
        } else if (!isStreaming) {
          setBusyState(false);
        }
      })
      .catch(function () {});
  }

  // Draggable logic
  var isDragging = false;
  var dragStartX = 0;
  var dragStartY = 0;
  var initialLeft = 0;
  var initialTop = 0;
  var hasMoved = false;

  function onPointerDown(e) {
    if (e.target === dismissBtn || e.target.closest('.mg-copilot-dismiss-btn')) return;
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
      var pillW = pill.offsetWidth || 110;
      var pillH = pill.offsetHeight || 36;
      var winW = window.innerWidth || document.documentElement.clientWidth || 1000;
      var winH = window.innerHeight || document.documentElement.clientHeight || 800;

      var nextX = Math.max(10, Math.min(winW - pillW - 10, initialLeft + dx));
      var nextY = Math.max(10, Math.min(winH - pillH - 10, initialTop + dy));

      root.style.bottom = 'auto';
      root.style.right = 'auto';
      root.style.left = nextX + 'px';
      root.style.top = nextY + 'px';

      // Smart drawer placement horizontally & vertically
      if (nextX < 460) {
        drawer.style.left = '0px';
        drawer.style.right = 'auto';
      } else {
        drawer.style.right = '0px';
        drawer.style.left = 'auto';
      }

      if (nextY > winH / 2) {
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

    if (hasMoved) {
      var rect = root.getBoundingClientRect();
      try {
        localStorage.setItem(STORAGE_POS_KEY, JSON.stringify({ left: rect.left, top: rect.top }));
      } catch(e) {}
    } else {
      toggleDrawer();
    }
  }

  pill.addEventListener('mousedown', onPointerDown);
  pill.addEventListener('touchstart', onPointerDown, { passive: true });

  dismissBtn.addEventListener('click', function (e) {
    e.stopPropagation();
    root.style.display = 'none';
    drawer.style.display = 'none';
  });

  closeBtn.addEventListener('click', function () {
    drawer.style.display = 'none';
  });

  clearBtn.addEventListener('click', function () {
    msgs = [];
    localStorage.removeItem(STORAGE_CHAT_KEY);
    renderMessages();
  });

  chips.forEach(function (chip) {
    chip.addEventListener('click', function () {
      var text = chip.getAttribute('data-text');
      textarea.value = text + ' ' + textarea.value;
      autoResizeTextarea();
      textarea.focus();
    });
  });

  // Stop current execution
  function stopExecution() {
    if (abortCtrl) abortCtrl.abort();
    var stopUrl = '/api/v1/projects/' + encodeURIComponent(project || 'current') + '/tasks/' + encodeURIComponent(taskId) + '/preview/stop';
    fetch(stopUrl, { method: 'POST' }).catch(function () {});
    isStreaming = false;
    setBusyState(false);

    if (msgs.length > 0 && msgs[msgs.length - 1].role === 'assistant') {
      var last = msgs[msgs.length - 1];
      last.isThinking = false;
      last.isDone = false;
      last.isStopped = true;
      if (!last.content) {
        last.content = '🛑 [已中止修改]';
      } else if (last.content.indexOf('[已中止修改]') === -1) {
        last.content += '\n🛑 [已中止修改]';
      }
      saveMessages();
      renderMessages();
    }
  }

  // Parse raw chunk event into friendly text / tool / thinking
  function processRawEvent(dataStr, assistantMsg) {
    if (dataStr === '{"type":"done"}') {
      assistantMsg.isThinking = false;
      assistantMsg.isDone = true;
      return true;
    }
    if (dataStr === '{"type":"stopped"}') {
      assistantMsg.isThinking = false;
      assistantMsg.isStopped = true;
      assistantMsg.content += '\n[已中止修改]';
      return true;
    }

    try {
      var wrapper = JSON.parse(dataStr);
      var raw = wrapper.payload || dataStr;
      var obj = typeof raw === 'string' && raw.startsWith('{') ? JSON.parse(raw) : (typeof raw === 'object' ? raw : null);

      if (obj) {
        // System thinking tokens -> keep as thinking
        if (obj.type === 'system' || obj.subtype === 'thinking_tokens') {
          assistantMsg.isThinking = true;
          return false;
        }

        // Assistant content
        if (obj.type === 'assistant' && obj.message && obj.message.content) {
          assistantMsg.isThinking = false;
          var arr = Array.isArray(obj.message.content) ? obj.message.content : [{ type: 'text', text: String(obj.message.content) }];
          arr.forEach(function (b) {
            if (b.type === 'text' && b.text) {
              assistantMsg.content = (assistantMsg.content ? assistantMsg.content + '\n' : '') + b.text;
            }
            if (b.type === 'tool_use' && b.name) {
              if (!assistantMsg.tools) assistantMsg.tools = [];
              var toolType = 'other';
              var target = '';
              var low = b.name.toLowerCase();
              if (low.indexOf('bash') !== -1 || low.indexOf('exec') !== -1 || low.indexOf('cmd') !== -1) {
                toolType = 'bash';
                target = (b.input && (b.input.command || b.input.cmd)) || '';
              } else if (low.indexOf('read') !== -1 || low.indexOf('view') !== -1) {
                toolType = 'read';
                target = (b.input && (b.input.file_path || b.input.path || b.input.AbsolutePath)) || '';
              } else if (low.indexOf('edit') !== -1 || low.indexOf('replace') !== -1 || low.indexOf('write') !== -1) {
                toolType = 'edit';
                target = (b.input && (b.input.file_path || b.input.path || b.input.TargetFile)) || '';
              } else if (low.indexOf('grep') !== -1 || low.indexOf('search') !== -1 || low.indexOf('find') !== -1) {
                toolType = 'search';
                target = (b.input && (b.input.pattern || b.input.Query || b.input.Pattern)) || '';
              } else {
                target = typeof b.input === 'string' ? b.input : JSON.stringify(b.input || '').slice(0, 40);
              }
              target = target.trim();

              var exists = assistantMsg.tools.some(function (t) {
                return (typeof t === 'object') ? (t.name === b.name && t.target === target) : (t === b.name + ' ' + target);
              });
              if (!exists) {
                assistantMsg.tools.push({
                  type: toolType,
                  name: b.name,
                  target: target || b.name
                });
              }
            }
          });
          return false;
        }

        // Result event
        if (obj.type === 'result' && obj.result) {
          assistantMsg.isThinking = false;
          if (!assistantMsg.content) {
            assistantMsg.content = obj.result;
          }
          return false;
        }
      } else if (typeof raw === 'string') {
        var clean = raw.trim();
        if (clean && !clean.startsWith('===') && !clean.startsWith('Command:') && !clean.startsWith('Started:') && !clean.startsWith('{')) {
          assistantMsg.content = (assistantMsg.content ? assistantMsg.content + '\n' : '') + clean;
        }
      }
    } catch(e) {}
    return false;
  }

  function autoResizeTextarea() {
    textarea.style.height = 'auto';
    var newH = Math.min(120, Math.max(52, textarea.scrollHeight));
    textarea.style.height = newH + 'px';
    submitBtn.style.height = newH + 'px';
    if (textarea.scrollHeight > 120) {
      textarea.style.overflowY = 'auto';
    } else {
      textarea.style.overflowY = 'hidden';
    }
  }

  textarea.addEventListener('input', autoResizeTextarea);

  // Send message stream
  function sendMessage() {
    if (isStreaming) {
      stopExecution();
      return;
    }

    var content = textarea.value.trim();
    if (!content && !selectedDOMTarget) return;

    var displayContent = content;
    var finalPrompt = content;

    if (selectedDOMTarget) {
      var domSummary = '@DOM(' + selectedDOMTarget.tag + ')';
      displayContent = domSummary + ' ' + (content || '请针对该元素进行优化');
      finalPrompt = `【目标页面 DOM 元素精准定位上下文】:
- CSS 选择器路径: ${selectedDOMTarget.selector}
- 标签与类名: <${selectedDOMTarget.tag}>
- 父级容器上下文: ${selectedDOMTarget.parent || '无'}
- 核心属性与配置: ${selectedDOMTarget.attrs || '无'}
- 页面可见文本/占位符: "${selectedDOMTarget.text || '无'}"
- HTML 代码结构片段:
\`\`\`html
${selectedDOMTarget.outerHTML}
\`\`\`

【用户代码优化需求】:
${content || '请根据上述目标 DOM 元素位置与代码上下文进行优化与修复'}`;
      clearSelectedDOM();
    }

    var userMsg = { role: 'user', content: displayContent };
    msgs.push(userMsg);
    textarea.value = '';
    autoResizeTextarea();

    var assistantMsg = { role: 'assistant', content: '', tools: [], isThinking: true, isDone: false, isStopped: false };
    msgs.push(assistantMsg);
    saveMessages();
    renderMessages();

    isStreaming = true;
    setBusyState(true);

    abortCtrl = new AbortController();
    var chatUrl = '/api/v1/projects/' + encodeURIComponent(project || 'current') + '/tasks/' + encodeURIComponent(taskId) + '/preview/chat';

    var historyPayload = msgs.slice(0, -2).map(function (m) {
      return { role: m.role, content: m.content };
    });

    fetch(chatUrl, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'Accept': 'text/event-stream'
      },
      body: JSON.stringify({ message: finalPrompt, history: historyPayload }),
      signal: abortCtrl.signal
    })
      .then(function (res) {
        if (!res.ok) {
          return res.text().then(function (rawText) {
            try {
              var data = JSON.parse(rawText);
              throw new Error(data.message || (data.error && data.error.message) || ('HTTP ' + res.status));
            } catch (parseErr) {
              if (parseErr.message && !parseErr.message.includes('JSON')) {
                throw parseErr;
              }
              throw new Error(rawText || ('HTTP ' + res.status));
            }
          });
        }
        var reader = res.body.getReader();
        var decoder = new TextDecoder();
        var buffer = '';

        function readChunk() {
          return reader.read().then(function (result) {
            if (result.done) {
              assistantMsg.isThinking = false;
              if (!assistantMsg.isStopped) assistantMsg.isDone = true;
              saveMessages();
              renderMessages();
              isStreaming = false;
              setBusyState(false);
              return;
            }
            buffer += decoder.decode(result.value, { stream: true });
            var parts = buffer.split('\n');
            buffer = parts.pop() || '';

            for (var i = 0; i < parts.length; i++) {
              var part = parts[i].trim();
              if (part.startsWith('data: ')) {
                var dataStr = part.slice(6);
                var isEnd = processRawEvent(dataStr, assistantMsg);
                renderMessages();
                if (isEnd) {
                  isStreaming = false;
                  setBusyState(false);
                  saveMessages();
                }
              }
            }
            return readChunk();
          });
        }
        return readChunk();
      })
      .catch(function (err) {
        if (err.name !== 'AbortError') {
          assistantMsg.isThinking = false;
          assistantMsg.content += '\n❌ 发生错误: ' + err.message;
          saveMessages();
          renderMessages();
        }
        isStreaming = false;
        setBusyState(false);
      });
  }

  submitBtn.addEventListener('click', sendMessage);

  // Keybinding: Enter adds newline; Cmd+Enter / Ctrl+Enter sends!
  textarea.addEventListener('keydown', function (e) {
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
      e.preventDefault();
      sendMessage();
    }
  });

  // DOM Inspector Implementation
  function getCssSelector(el) {
    var path = [];
    while (el && el.nodeType === Node.ELEMENT_NODE && el !== document.body && el !== document.documentElement) {
      var selector = el.nodeName.toLowerCase();
      if (el.id) {
        selector += '#' + el.id;
        path.unshift(selector);
        break;
      } else if (el.className && typeof el.className === 'string' && el.className.trim()) {
        selector += '.' + el.className.trim().split(/\s+/)[0];
      }
      var parent = el.parentNode;
      if (parent) {
        var siblings = Array.prototype.filter.call(parent.children, function(e) { return e.nodeName === el.nodeName; });
        if (siblings.length > 1) {
          var index = Array.prototype.indexOf.call(siblings, el) + 1;
          selector += ':nth-of-type(' + index + ')';
        }
      }
      path.unshift(selector);
      el = el.parentElement;
    }
    return path.join(' > ');
  }

  function startInspector() {
    isInspectorActive = true;
    drawer.style.display = 'none';
    ensureTopMost();
    inspectorBar.style.display = 'flex';
    document.addEventListener('mousemove', onInspectorMouseMove, true);
    document.addEventListener('click', onInspectorClick, true);
  }

  function stopInspector() {
    isInspectorActive = false;
    inspectorOverlay.style.display = 'none';
    inspectorBar.style.display = 'none';
    document.removeEventListener('mousemove', onInspectorMouseMove, true);
    document.removeEventListener('click', onInspectorClick, true);
    drawer.style.display = 'flex';
    textarea.focus();
  }

  function onInspectorMouseMove(e) {
    if (!isInspectorActive) return;
    var el = document.elementFromPoint(e.clientX, e.clientY);
    if (!el || el.closest('.mg-copilot-root') || el.closest('.mg-inspector-bar') || el.closest('.mg-inspector-overlay')) {
      inspectorOverlay.style.display = 'none';
      return;
    }

    var rect = el.getBoundingClientRect();
    inspectorOverlay.style.display = 'block';
    inspectorOverlay.style.top = rect.top + 'px';
    inspectorOverlay.style.left = rect.left + 'px';
    inspectorOverlay.style.width = rect.width + 'px';
    inspectorOverlay.style.height = rect.height + 'px';

    var tagStr = el.tagName.toLowerCase();
    if (el.id) tagStr += '#' + el.id;
    else if (el.className && typeof el.className === 'string' && el.className.trim()) {
      tagStr += '.' + el.className.trim().split(/\s+/).slice(0, 2).join('.');
    }
    inspectorBadge.textContent = '<' + tagStr + '> ' + Math.round(rect.width) + '×' + Math.round(rect.height);

    if (rect.top < 26) {
      inspectorBadge.style.top = 'auto';
      inspectorBadge.style.bottom = '-22px';
    } else {
      inspectorBadge.style.top = '-22px';
      inspectorBadge.style.bottom = 'auto';
    }
  }

  function extractElementContext(el) {
    var tag = el.tagName.toLowerCase();
    var id = el.id ? '#' + el.id : '';
    var classes = (typeof el.className === 'string' && el.className.trim()) ? '.' + el.className.trim().split(/\s+/).slice(0, 2).join('.') : '';
    var tagDisplay = tag + (id ? id : classes);

    // 1. Text / value / placeholder
    var text = (el.innerText || el.textContent || '').trim().slice(0, 80);
    if (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA') {
      text = el.value || el.placeholder || text;
    }

    // 2. Attributes
    var attrs = [];
    ['id', 'name', 'type', 'placeholder', 'role', 'aria-label', 'href', 'data-testid'].forEach(function (k) {
      if (el.hasAttribute && el.hasAttribute(k)) {
        attrs.push(k + '="' + el.getAttribute(k) + '"');
      }
    });

    // 3. Parent container context
    var parentInfo = '';
    if (el.parentElement && el.parentElement !== document.body && el.parentElement !== document.documentElement) {
      var p = el.parentElement;
      var pTag = p.tagName.toLowerCase() + (p.id ? '#' + p.id : (p.className && typeof p.className === 'string' && p.className.trim() ? '.' + p.className.trim().split(/\s+/)[0] : ''));
      var pText = (p.innerText || '').trim().slice(0, 24);
      parentInfo = '<' + pTag + '>' + (pText ? ' "' + pText + '"' : '');
    }

    // 4. Selector
    var selector = getCssSelector(el);

    // 5. Clean outerHTML snippet
    var outerHTML = (el.outerHTML || '').slice(0, 260);

    return {
      tag: tagDisplay,
      tagName: tag,
      id: el.id || '',
      name: el.getAttribute ? (el.getAttribute('name') || '') : '',
      text: text,
      attrs: attrs.join(' '),
      parent: parentInfo,
      selector: selector,
      outerHTML: outerHTML
    };
  }

  function onInspectorClick(e) {
    if (!isInspectorActive) return;
    var el = document.elementFromPoint(e.clientX, e.clientY);
    if (el && el.closest('.mg-inspector-bar')) return;

    e.preventDefault();
    e.stopPropagation();

    if (el && !el.closest('.mg-copilot-root') && !el.closest('.mg-inspector-bar') && !el.closest('.mg-inspector-overlay')) {
      selectedDOMTarget = extractElementContext(el);
      updateDOMTagUI();
    }

    stopInspector();
  }

  function updateDOMTagUI() {
    if (!selectedDOMTarget) {
      domTagContainer.style.display = 'none';
      domTagContainer.innerHTML = '';
      return;
    }
    domTagContainer.style.display = 'block';
    domTagContainer.innerHTML = `
      <div class="mg-dom-tag-badge">
        <div class="mg-dom-tag-left">
          <span class="mg-dom-tag-label">🎯 @DOM</span>
          <span class="mg-dom-tag-name">(${escapeHtml(selectedDOMTarget.tag)})</span>
        </div>
        <div class="mg-dom-hover-card">
          <div class="mg-dom-card-header">🎯 目标元素定位上下文</div>
          <div class="mg-dom-card-row">
            <span class="mg-dom-card-k">选择器路径:</span>
            <code class="mg-dom-card-code">${escapeHtml(selectedDOMTarget.selector)}</code>
          </div>
          ${selectedDOMTarget.text ? `
            <div class="mg-dom-card-row">
              <span class="mg-dom-card-k">内容/占位:</span>
              <span class="mg-dom-card-v">"${escapeHtml(selectedDOMTarget.text)}"</span>
            </div>
          ` : ''}
          ${selectedDOMTarget.parent ? `
            <div class="mg-dom-card-row">
              <span class="mg-dom-card-k">父级容器:</span>
              <code class="mg-dom-card-code">${escapeHtml(selectedDOMTarget.parent)}</code>
            </div>
          ` : ''}
          <div class="mg-dom-card-row">
            <span class="mg-dom-card-k">HTML 结构:</span>
            <pre class="mg-dom-card-pre"><code>${escapeHtml(selectedDOMTarget.outerHTML)}</code></pre>
          </div>
        </div>
        <span class="mg-dom-tag-remove" title="取消元素关联">&times;</span>
      </div>
    `;
    domTagContainer.querySelector('.mg-dom-tag-remove').addEventListener('click', clearSelectedDOM);
  }

  function clearSelectedDOM() {
    selectedDOMTarget = null;
    updateDOMTagUI();
  }

  inspectBtn.addEventListener('click', startInspector);
  inspectorBar.querySelector('.mg-inspector-bar-cancel').addEventListener('click', stopInspector);

  // Initial render & auto-reconnect on refresh
  renderMessages();
  checkStatus();

  // Track page unload to prevent transient fetch errors from polluting chat history
  window.addEventListener('beforeunload', function () {
    window.isUnloading = true;
  });

  // Global Keyboard Shortcuts
  window.addEventListener('keydown', function (e) {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
      e.preventDefault();
      toggleWidgetVisibility();
    } else if (e.key === 'Escape') {
      if (isInspectorActive) {
        stopInspector();
      } else if (drawer.style.display === 'flex') {
        drawer.style.display = 'none';
      }
    }
  });

  // Ensure copilot widget stays at the top of the body or active modal dialog
  function ensureTopMost(force) {
    if (isDragging && !force) return;
    var openDialog = document.querySelector('dialog[open]');
    var targetParent = openDialog || document.body;
    if (!targetParent) return;

    if (root.parentElement !== targetParent) {
      targetParent.appendChild(root);
    } else if (targetParent.lastElementChild !== root &&
               targetParent.lastElementChild !== inspectorBar &&
               targetParent.lastElementChild !== inspectorOverlay) {
      targetParent.appendChild(root);
    }

    if (inspectorOverlay.parentElement !== targetParent) {
      targetParent.appendChild(inspectorOverlay);
    }
    if (inspectorBar.parentElement !== targetParent) {
      targetParent.appendChild(inspectorBar);
    }
  }

  if (window.MutationObserver && document.documentElement) {
    var dialogObserver = new MutationObserver(function (mutations) {
      if (isDragging || isInspectorActive) return;
      for (var i = 0; i < mutations.length; i++) {
        var m = mutations[i];
        if (m.type === 'attributes' && (m.attributeName === 'open' || m.attributeName === 'aria-modal' || m.attributeName === 'class')) {
          ensureTopMost();
          return;
        }
        if (m.addedNodes && m.addedNodes.length > 0) {
          for (var j = 0; j < m.addedNodes.length; j++) {
            var n = m.addedNodes[j];
            if (n !== root && n !== inspectorBar && n !== inspectorOverlay && n.nodeType === 1) {
              ensureTopMost();
              return;
            }
          }
        }
      }
    });
    dialogObserver.observe(document.documentElement, {
      childList: true,
      subtree: true,
      attributes: true,
      attributeFilter: ['open', 'aria-modal', 'class']
    });
  }
})();
