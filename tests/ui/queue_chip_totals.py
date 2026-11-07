"""Browser regression check: python3 tests/ui/queue_chip_totals.py (requires Playwright Chromium).

ix.12.14: the Downloads page lists the next 1,000 files to download and 500
set aside. When the whole queue holds more, the "Queued" and "Set aside by
the archive" chips say "N of total"; when the page lists everything they show
the plain count, also after downloads start (the total is taken when the page
loads, so it must not turn into "2 of 3"). Runs the real queue.js and page
markup with stubbed imports; invented data only.
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
  async boot(snap) {
    SNAP = snap;
    document.getElementById('page-queue').classList.remove('hidden');
    initQueue();
    await showQueuePage();
  },
  async reload(snap) { SNAP = snap; await loadSnapshot(); },
  start(i) { handleWs({ type: 'download_start', payload: job(i, 'active') }); },
  chip: (id) => document.querySelector(`#queue-chips [data-chip="${id}"] [data-chip-count]`).textContent,
  job,
};
'''

with sync_playwright() as pw:
    browser = pw.chromium.launch(headless=True)
    page = browser.new_page(viewport={'width': 1280, 'height': 900}, locale='en-US')
    errors = []
    page.on('pageerror', lambda e: errors.append(str(e)))
    page.on('console', lambda m: errors.append(m.text) if m.type == 'error' else None)
    page.set_content(f'<!doctype html><html><head><style>{css}</style></head><body><div id="content-area">{markup}</div></body></html>')
    page.add_script_tag(content=prelude + source + harness)
    h = 'window.queueHarness'
    settle = lambda: page.wait_for_timeout(300)

    # The page lists 3 of 4,832 files to download and 2 of 500 set aside.
    page.evaluate(f'''{h}.boot({{ active: [], recent: [], queuedCount: 4832, archiveHeldCount: 500, personPausedCount: 0,
      queued: [{h}.job(1, 'queued'), {h}.job(2, 'queued'), {h}.job(3, 'queued'),
               {h}.job(4, 'paused', {{ archiveHold: 'copy' }}), {h}.job(5, 'paused', {{ archiveHold: 'deadline' }})] }})''')
    settle()
    assert page.evaluate(f'{h}.chip("queued")') == '3 of 4,832', page.evaluate(f'{h}.chip("queued")')
    assert page.evaluate(f'{h}.chip("held")') == '2 of 500', page.evaluate(f'{h}.chip("held")')

    # Everything listed: plain counts, and still plain after a download starts.
    page.evaluate(f'''{h}.reload({{ active: [], recent: [], queuedCount: 3, archiveHeldCount: 0, personPausedCount: 0,
      queued: [{h}.job(1, 'queued'), {h}.job(2, 'queued'), {h}.job(3, 'queued')] }})''')
    settle()
    assert page.evaluate(f'{h}.chip("queued")') == '3', page.evaluate(f'{h}.chip("queued")')
    page.evaluate(f'{h}.start(1)')
    settle()
    assert page.evaluate(f'{h}.chip("queued")') == '2', page.evaluate(f'{h}.chip("queued")')
    assert page.evaluate(f'{h}.chip("held")') == '0', page.evaluate(f'{h}.chip("held")')

    assert not errors, errors
    print('queue chip totals: ok')
    browser.close()
