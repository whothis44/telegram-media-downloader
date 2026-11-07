"""Browser regression check: python3 tests/ui/queue_download_order.py (requires Playwright Chromium).

The Queue page lists files in the order they download by default (ix.12.3):
downloading first, then the queue in the server's claim order (its `order`),
then paused rows. A file that starts goes to the top of the downloading rows
and the others keep their places; Done and Failed call their default "Finished
time". Runs the real queue.js and page markup with stubbed imports; invented
data only.
"""
from pathlib import Path
import re
from playwright.sync_api import sync_playwright

root = Path(__file__).resolve().parents[2]
public = root / 'core-service/internal/webassets/public'
html = (public / 'index.html').read_text(encoding='utf-8')
markup = html[html.index('<div id="page-queue"'):html.index('<!-- Tools group page')]
css = (public / 'css/tailwind.css').read_text(encoding='utf-8') + '\n' + (public / 'css/main.css').read_text(encoding='utf-8')
source = (public / 'js/queue.js').read_text(encoding='utf-8')
source = re.sub(r'^import\s[\s\S]*?;\n', '', source, flags=re.M)
source = re.sub(r'^export ', '', source, flags=re.M)
prelude = '''
const ws = { on() {} };
let SNAP = { active: [], queued: [], recent: [] };
const api = { get: async () => SNAP, post: async () => ({ success: true }) };
const showToast = () => {};
const escapeHtml = (s) => String(s).replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('>','&gt;').replaceAll('"','&quot;').replaceAll("'",'&#39;');
const formatBytes = (n) => `${n} B`;
const getFileIcon = () => 'ri-file-line';
const i18nT = (key, fallback) => fallback ?? key;
const i18nTf = (key, values, fallback) => Object.entries(values || {}).reduce((s, [k, v]) => s.replaceAll('{' + k + '}', v), fallback ?? key);
const getGroupName = (id, options) => options?.fallback || id;
const fileTokenQuery = () => '';
const confirmSheet = async () => true;
const openSheet = () => ({ root: document.createElement('div'), close() {} });
'''
harness = '''
const G = '-1009000000001';
const job = (i, status, extra) => ({ key: G + '_' + i, groupId: G, groupName: 'Invented Chat', messageId: i,
  fileName: 'file_' + i + '.mp4', fileSize: 1000, progress: 0, received: 0, total: 1000, bps: 0,
  eta: null, status, addedAt: 1700000000000 + i, mediaType: 'video', ...extra });
window.queueHarness = {
  async boot() {
    // Added in the order 1..6, but the ranking downloads 5, 2, 6 next; 4 is paused.
    SNAP = {
      active: [job(9, 'active'), job(8, 'active')],
      queued: [job(5, 'queued', { order: 0 }), job(2, 'queued', { order: 1 }), job(6, 'queued', { order: 2 }), job(4, 'paused', { order: 3 })],
      recent: [],
    };
    document.getElementById('page-queue').classList.remove('hidden');
    initQueue();
    await showQueuePage();
  },
  start(i) { handleWs({ type: 'download_start', payload: job(i, 'active') }); },
  keys: () => Array.from(document.getElementById('queue-rows').children).map((r) => Number(r.dataset.key.split('_').pop())),
};
'''

with sync_playwright() as pw:
    browser = pw.chromium.launch(headless=True)
    page = browser.new_page(viewport={'width': 1280, 'height': 900})
    errors = []
    page.on('pageerror', lambda e: errors.append(str(e)))
    page.on('console', lambda m: errors.append(m.text) if m.type == 'error' else None)
    page.set_content(f'<!doctype html><html><head><style>{css}</style></head><body><div id="content-area">{markup}</div></body></html>')
    page.add_script_tag(content=prelude + source + harness)
    h = 'window.queueHarness'
    settle = lambda: page.wait_for_timeout(300)

    page.evaluate(f'{h}.boot()')
    settle()
    assert page.input_value('#queue-sort') == 'download', page.input_value('#queue-sort')
    keys = page.evaluate(f'{h}.keys()')
    assert keys == [9, 8, 5, 2, 6, 4], keys

    # 5 starts: it goes to the top of the downloading rows; the rest keep their order.
    page.mouse.move(0, 0)
    page.evaluate(f'{h}.start(5)')
    page.wait_for_timeout(1800)  # FOLLOW_TOP_MS, then a frame
    keys = page.evaluate(f'{h}.keys()')
    assert keys == [5, 9, 8, 2, 6, 4], keys

    # Added time is still there, newest first, as before.
    page.select_option('#queue-sort', 'addedAt')
    settle()
    keys = page.evaluate(f'{h}.keys()')
    assert keys == [9, 8, 6, 5, 4, 2], keys

    # Done and Failed: the same option says what it sorts by.
    assert page.inner_text('#queue-sort option[value="addedAt"]') == 'Added time'
    page.click('#queue-chips [data-chip="done"]')
    settle()
    assert page.inner_text('#queue-sort option[value="addedAt"]') == 'Finished time', page.inner_text('#queue-sort option[value="addedAt"]')
    page.click('#queue-chips [data-chip="current"]')
    settle()
    assert page.inner_text('#queue-sort option[value="addedAt"]') == 'Added time'

    assert not errors, errors
    print('queue download order: ok')
    browser.close()
