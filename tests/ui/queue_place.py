"""Browser regression check: python3 tests/ui/queue_place.py (requires Playwright Chromium).

The Queue list keeps the reader's place (js/queue.js reconcileRows):
rows arriving above the viewport don't move the visible rows, rows arriving
at the top while the pointer is over the list wait behind the "N new — show"
pill, at the very top with the pointer elsewhere the list follows the live
order, rows leaving above the viewport don't move it either, and "Pause list
updates" holds arrivals while the counters keep updating. Runs the real
queue.js and page markup with stubbed imports; invented data only.
"""
from pathlib import Path
import re
from playwright.sync_api import sync_playwright

root = Path(__file__).resolve().parents[2]
public = root / 'core-service/internal/webassets/public'
html = (public / 'index.html').read_text()
markup = html[html.index('<div id="page-queue"'):html.index('<!-- Tools group page')]
css = (public / 'css/tailwind.css').read_text() + '\n' + (public / 'css/main.css').read_text()
source = (public / 'js/queue.js').read_text()
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
const job = (i) => ({ key: G + '_' + i, groupId: G, groupName: 'Invented Chat', messageId: i,
  fileName: 'file_' + i + '.jpg', fileSize: 1000, progress: 0, received: 0, total: 1000, bps: 0,
  eta: null, status: 'queued', addedAt: 1700000000000 + i, mediaType: 'photo' });
window.queueHarness = {
  async boot(n) {
    SNAP = { active: [], queued: Array.from({ length: n }, (_, i) => job(i + 1)), recent: [] };
    document.getElementById('page-queue').classList.remove('hidden');
    initQueue();
    await showQueuePage();
  },
  arrive(from, n) {
    for (let i = from; i < from + n; i++) handleWs({ type: 'download_start', payload: { ...job(i), fileSize: 1000 } });
  },
  finish(keys) {
    for (const k of keys) {
      const m = Number(k.split('_').pop());
      handleWs({ type: 'download_complete', payload: { ...job(m), filePath: 'Invented Chat/f.jpg' } });
    }
  },
  firstVisible() {
    const vp = document.getElementById('queue-viewport');
    for (const r of document.getElementById('queue-rows').children) {
      if (r.offsetTop + r.offsetHeight > vp.scrollTop) return { key: r.dataset.key, y: r.getBoundingClientRect().top };
    }
    return null;
  },
  keysAbove() {
    const vp = document.getElementById('queue-viewport');
    return Array.from(document.getElementById('queue-rows').children)
      .filter((r) => r.offsetTop + r.offsetHeight <= vp.scrollTop).map((r) => r.dataset.key);
  },
  pill() {
    const w = document.getElementById('queue-new-pill');
    return w.classList.contains('hidden') ? null : document.getElementById('queue-new-pill-btn').textContent;
  },
  chip(id) { return Number(document.querySelector(`#queue-chips [data-chip="${id}"] [data-chip-count]`).textContent); },
  shown: () => _order.length,
  firstRow: () => document.getElementById('queue-rows').firstElementChild.dataset.key,
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
    settle = lambda: page.wait_for_timeout(250)

    page.evaluate(f'{h}.boot(400)')
    settle()
    vp = page.locator('#queue-viewport')
    # Load pages down the list, then sit halfway.
    for _ in range(12):
        vp.evaluate('v => { v.scrollTop = v.scrollHeight; }')
        page.wait_for_timeout(120)
    vp.evaluate('v => { v.scrollTop = Math.round(v.scrollHeight / 2); }')
    settle()
    before = page.evaluate(f'{h}.firstVisible()')
    assert before, 'no visible row'

    # 1. 200 rows arrive at the top while scrolled halfway: nothing moves.
    page.evaluate(f'{h}.arrive(1001, 200)')
    settle()
    after = page.evaluate(f'{h}.firstVisible()')
    assert after['key'] == before['key'] and abs(after['y'] - before['y']) < 1, (before, after)
    assert page.evaluate(f'{h}.shown()') == 600, page.evaluate(f'{h}.shown()')
    assert page.evaluate(f'{h}.pill()') is None, page.evaluate(f'{h}.pill()')
    assert page.evaluate(f'{h}.chip("all")') == 600

    # 2. Rows above the viewport finish (leave the Active view): still put.
    above = page.evaluate(f'{h}.keysAbove()')[:50]
    assert len(above) == 50, len(above)
    page.evaluate(f'{h}.finish({above!r})')
    settle()
    after2 = page.evaluate(f'{h}.firstVisible()')
    assert after2['key'] == before['key'] and abs(after2['y'] - before['y']) < 1, (before, after2)

    # 3. "Pause list updates": arrivals wait, counters keep counting.
    page.click('#queue-freeze')
    assert page.get_attribute('#queue-freeze', 'aria-pressed') == 'true'
    shown = page.evaluate(f'{h}.shown()')
    page.evaluate(f'{h}.arrive(2001, 10)')
    settle()
    assert page.evaluate(f'{h}.shown()') == shown
    assert page.evaluate(f'{h}.pill()') == '10 new — show', page.evaluate(f'{h}.pill()')
    assert page.evaluate(f'{h}.chip("all")') == 610
    frozen = page.evaluate(f'{h}.firstVisible()')
    assert frozen['key'] == before['key'] and abs(frozen['y'] - before['y']) < 1, (before, frozen)
    page.click('#queue-freeze')  # resume: they go in above the reader
    settle()
    assert page.get_attribute('#queue-freeze', 'aria-pressed') == 'false'
    assert page.evaluate(f'{h}.pill()') is None
    resumed = page.evaluate(f'{h}.firstVisible()')
    assert resumed['key'] == before['key'] and abs(resumed['y'] - before['y']) < 1, (before, resumed)

    # 4. Scrolling back up brings back the rows the window dropped off the
    # top, without jumps; at the top with the pointer on the list, new rows
    # don't push the list down: they wait behind the pill.
    for _ in range(12):  # past MAX_DOM_ROWS, so the window drops rows off the top
        vp.evaluate('v => { v.scrollTop = v.scrollHeight; }')
        page.wait_for_timeout(120)
    assert page.evaluate(f'{h}.firstRow() !== _order[0]'), 'window never slid'
    vp.evaluate('v => { v.scrollTop = 100; }')
    near = page.evaluate(f'{h}.firstVisible()')
    settle()
    assert page.evaluate('document.getElementById("queue-viewport").scrollTop') > 100, 'rows above did not come back'
    back = page.evaluate(f'{h}.firstVisible()')
    assert back['key'] == near['key'] and abs(back['y'] - near['y']) < 1, (near, back)
    for _ in range(30):
        vp.evaluate('v => { v.scrollTop = Math.max(0, v.scrollTop - 1500); }')
        page.wait_for_timeout(60)
        if page.evaluate(f'{h}.firstRow() === _order[0] && document.getElementById("queue-viewport").scrollTop === 0'):
            break
    else:
        raise AssertionError('never got back to the top of the list')
    settle()
    page.hover('#queue-viewport')
    settle()
    top = page.evaluate(f'{h}.firstVisible()')
    page.evaluate(f'{h}.arrive(3001, 5)')
    settle()
    still = page.evaluate(f'{h}.firstVisible()')
    assert still['key'] == top['key'] and abs(still['y'] - top['y']) < 1, (top, still)
    assert page.evaluate(f'{h}.pill()') == '5 new — show', page.evaluate(f'{h}.pill()')
    page.click('#queue-new-pill-btn')
    settle()
    assert page.evaluate(f'{h}.firstRow()') == '-1009000000001_3005', page.evaluate(f'{h}.firstRow()')
    assert page.evaluate(f'{h}.pill()') is None

    # 5. At the very top with the pointer away from the list, nobody is reading
    # a particular row: arrivals appear by themselves, with no pill and no click.
    page.mouse.move(0, 0)
    settle()
    page.evaluate(f'{h}.arrive(4001, 3)')
    page.wait_for_timeout(1800)  # FOLLOW_TOP_MS, then a frame
    assert page.evaluate(f'{h}.firstRow()') == '-1009000000001_4003', page.evaluate(f'{h}.firstRow()')
    assert page.evaluate(f'{h}.pill()') is None
    # ... and rows that finish leave by themselves too.
    page.evaluate(f"{h}.finish(['-1009000000001_4003'])")
    page.wait_for_timeout(1800)
    assert page.evaluate(f'{h}.firstRow()') == '-1009000000001_4002', page.evaluate(f'{h}.firstRow()')

    assert not errors, errors
    print('queue keeps the reader\'s place: ok')
    browser.close()
