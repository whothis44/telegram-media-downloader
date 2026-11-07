"""Browser regression check: python3 tests/ui/archive_rescan_lanes.py (requires Playwright Chromium).

ix.12.14, the Rescan tab: who holds each lane, since when and its last sign of
work ("no progress for N min" once quiet for 10 minutes); waiting rows say how
many groups come before them and are listed in that order; ranked rows show
their rank. Choose groups can hide channels and supergroups (chats of unknown
type stay), ticks survive the filter and the page says how many ticked are
hidden. Settings has "Check for updates". Runs the real ix.html with the API
answered from invented data.
"""
import json
import time
from pathlib import Path
from playwright.sync_api import sync_playwright

root = Path(__file__).resolve().parents[2]
html = (root / 'core-service/internal/app/ixassets/ix.html').read_text(encoding='utf-8')
ORIGIN = 'http://archive.test'
now = int(time.time() * 1000)


def item(gid, name, state, phase='messages', **kw):
    d = {'groupId': gid, 'name': name, 'state': state, 'phase': phase, 'attempts': 0, 'processed': 0, 'downloaded': 0}
    d.update(kw)
    return d


status = {
    'version': 'test', 'queueLeft': {}, 'dlState': {}, 'account': {}, 'export': {}, 'freeze': {}, 'held': {}, 'dedup': {},
    'copies': {}, 'downloads': {}, 'dups': {}, 'readonly': {},
    'settings': {'stageKeepFreeGb': 0},
    'staging': {'on': True, 'keepFreeGb': 50, 'roomKnown': True, 'room': 120 << 30, 'waiting': 3, 'bytes': 5 << 20},
    'update': {'current': '3.4.18-ix.12.14', 'latest': '3.4.18-ix.12.14', 'newer': False, 'checkedAt': now - 60000, 'restarts': True},
    'groups': [
        {'id': '111', 'name': 'Alpha Chan', 'configured': True, 'kind': 'channel', 'files': 0},
        {'id': '222', 'name': 'Beta Super', 'configured': True, 'kind': 'forum', 'files': 0},
        {'id': '333', 'name': 'Gamma Group', 'configured': True, 'kind': 'group', 'files': 0},
        {'id': '444', 'name': 'Delta Unknown', 'configured': True, 'files': 0},
    ],
    'queue': {'running': True, 'items': [
        item('w2', 'Waiting Second', 'waiting'),
        item('stuck', 'Stuck Reader', 'running', startedAt=now - 19 * 3600 * 1000, processed=100),
        item('w1', 'Waiting First', 'waiting'),
        item('f1', 'Files Holder', 'running', phase='files', startedAt=now - 600000),
    ]},
    'lanes': {
        'messages': {'groupId': 'stuck', 'name': 'Stuck Reader', 'since': now - 19 * 3600 * 1000, 'lastWork': now - 19 * 3600 * 1000, 'place': 38},
        'files': {'groupId': 'f1', 'name': 'Files Holder', 'since': now - 600000, 'lastWork': now - 5000, 'place': 36},
        'ahead': {'w1': 0, 'w2': 1}, 'places': {'w1': 45, 'w2': 46, 'stuck': 38, 'f1': 36}, 'stallAfter': 1800000,
    },
}
update_posts = []


def handle(route, request):
    url = request.url.replace(ORIGIN, '')
    if url == '/ix' or url.startswith('/ix#'):
        return route.fulfill(status=200, content_type='text/html', body=html)
    if url.startswith('/api/ix/status'):
        return route.fulfill(status=200, content_type='application/json', body=json.dumps(status))
    if url.startswith('/api/ix/update'):
        update_posts.append(json.loads(request.post_data))
        u = dict(status['update'], checkedAt=int(time.time() * 1000))
        return route.fulfill(status=200, content_type='application/json', body=json.dumps(u))
    return route.fulfill(status=200, content_type='application/json', body='{}')


with sync_playwright() as p:
    browser = p.chromium.launch()
    page = browser.new_page(viewport={'width': 1200, 'height': 900})
    errors = []
    page.on('pageerror', lambda e: errors.append(str(e)))
    page.route(ORIGIN + '/**', handle)
    page.goto(ORIGIN + '/ix#rescan')
    page.wait_for_selector('[data-pick]')
    body = page.inner_text('#app')
    assert 'Messages lane: Stuck Reader (rank 38)' in body, body
    assert 'no progress for 1,140 min' in body, body
    assert 'Files lane: Files Holder (rank 36)' in body and 'last sign of work just now' in body, body
    # Rows in run order: running first, then the waiting ones by place in their lane.
    rows = page.eval_on_selector_all('table tr td:nth-child(2)', 'c => c.map(x => x.innerText.split("\\n")[0])')
    assert rows[:4] == ['Stuck Reader', 'Files Holder', 'Waiting First', 'Waiting Second'], rows
    assert 'next in its lane' in body and '1 before it in its lane' in body, body
    assert 'Rank 45: change in Priorities' in body, body

    # The type filter: hide channels and supergroups (a forum is one); unknown stays.
    names = lambda: page.eval_on_selector_all('[data-pick]', 'c => c.map(x => x.getAttribute("data-name"))')
    assert set(names()) == {'Alpha Chan', 'Beta Super', 'Gamma Group', 'Delta Unknown'}, names()
    page.check('[data-pick="111"]')
    page.check('[data-pick-hide="channel"]')
    page.check('[data-pick-hide="supergroup"]')
    assert set(names()) == {'Gamma Group', 'Delta Unknown'}, names()
    body = page.inner_text('#app')
    assert '1 ticked chat(s) are hidden by this' in body and '1 ticked' in body, body
    # Remembered after a reload.
    page.reload()
    page.wait_for_selector('[data-pick]')
    assert set(names()) == {'Gamma Group', 'Delta Unknown'}, names()
    page.uncheck('[data-pick-hide="channel"]')
    assert 'Alpha Chan' in names(), names()

    # Settings: Check for updates.
    page.goto(ORIGIN + '/ix#settings')
    page.wait_for_selector('[data-act="u-check"]')
    assert 'You have the latest (3.4.18-ix.12.14)' in page.inner_text('#app')
    assert 'Keep at least' in page.inner_text('#app') and 'Staging may use about 120.00 GB more now' in page.inner_text('#app')
    page.click('[data-act="u-check"]')
    page.wait_for_timeout(300)
    assert update_posts == [{'action': 'check'}], update_posts
    assert not errors, errors
    browser.close()
print('archive rescan lanes: ok')
