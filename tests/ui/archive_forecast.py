"""Browser regression check: python3 tests/ui/archive_forecast.py (requires Playwright Chromium).

ix.12.9: the Overview's "Before the deadline" card shows the forecast to the
deletion date (ix_forecast.go) when asked: both speeds, the stages, sizes and
groups, and the files not queued yet. Runs the real ix.html with the API
answered from invented data.
"""
import json
import os
import time
from pathlib import Path
from playwright.sync_api import sync_playwright

root = Path(__file__).resolve().parents[2]
html = (root / 'core-service/internal/app/ixassets/ix.html').read_text(encoding='utf-8')
ORIGIN = 'http://archive.test'
MB = 1 << 20
until = int(time.time()) + 20 * 86400

settings = {'pace': 'normal', 'quietStart': '', 'quietEnd': '', 'pauseOnFreeze': False, 'deadlineOrder': 'auto',
            'downloadPacing': True, 'autoRetry': True, 'holdLikely': True, 'holdForever': False, 'previewsAll': 'auto',
            'exportEnabled': True, 'exportTime': '03:30', 'exportDir': ''}
group = {'id': 'g1', 'name': 'Invented Chat', 'dir': 'g1', 'configured': True, 'messages': 10, 'deleted': 0, 'gone': 0,
         'tgCount': 10, 'lastWalk': 1, 'filesToGo': 0, 'files': 3, 'finishedAt': 0}
deadline = {'mode': 'auto', 'active': True, 'until': until, 'leftFiles': 40, 'leftBytes': 9000 * MB, 'toGetFiles': 50,
            'toGetBytes': 9500 * MB, 'rate24h': 100000, 'rate3h': 50000, 'rate1h': 50000, 'rate': 50000, 'rateUsed': '3h',
            'premium1h': 300, 'etaSec': 200000, 'finishes': 'no', 'deferred': [0, 3, 0, 0, 0, 0, 0, 0]}
status = {'version': '1.0.0', 'groups': [group], 'queue': {'items': []}, 'queueLeft': {}, 'dlState': {}, 'settings': settings,
          'account': {}, 'export': {}, 'freeze': {'until': until}, 'held': {}, 'dedup': {}, 'copies': {'queue': {}, 'sampled': {}},
          'downloads': {}, 'dups': {}, 'readonly': {}, 'fixity': {}, 'staging': {}, 'deadline': deadline,
          'update': {'current': '1.0.0', 'latest': '1.0.0', 'newer': False}}


def row(name, files, size, d0, b0, d1, b1, **kw):
    r = {'name': name, 'files': files, 'bytes': size * MB, 'doneFiles': [d0, d1], 'doneBytes': [b0 * MB, b1 * MB]}
    r.update(kw)
    return r


forecast = {
    'until': until, 'leftSec': 20 * 86400,
    'rates': [{'name': 'last24h', 'rate': 3e6, 'hours': 24, 'budget': 9000 * MB, 'finishes': False, 'spare': 0},
              {'name': 'limited', 'rate': 1e6, 'hours': 16, 'budget': 4000 * MB, 'finishes': False, 'spare': 0}],
    'total': row('', 40, 9000, 38, 8800, 30, 4000),
    'stages': [row('downloading now', 2, 100, 2, 100, 2, 100), row('ranked groups and other files first', 10, 900, 10, 900, 10, 900),
               row('probable copies held back', 28, 8000, 26, 7800, 18, 3000)],
    'bands': [row('under 10 MB', 30, 100, 30, 100, 30, 100), row('1 GB or more', 10, 8900, 8, 8700, 0, 0)],
    'groups': [row('Invented Ranked', 12, 1000, 12, 1000, 12, 1000, groupId='g2', place=1, private=True),
               row('Invented Chat', 28, 8000, 26, 7800, 18, 3000, groupId='g1', private=False)],
    'paused': 1, 'unqueued': 5, 'unqueuedBytes': 500 * MB, 'unqueuedFit': [500 * MB, 0],
}
loss = dict(deadline, loss=[{'groupId': 'g1', 'name': 'Invented Chat', 'private': False, 'files': 28, 'bytes': 8000 * MB, 'scanned': True}], forecast=forecast)


def handle(route, request):
    url = request.url.replace(ORIGIN, '')
    if url == '/ix' or url.startswith('/ix#'):
        return route.fulfill(status=200, content_type='text/html', body=html)
    if url.startswith('/api/ix/deadline'):
        return route.fulfill(status=200, content_type='application/json', body=json.dumps(loss))
    if url.startswith('/api/ix/status'):
        return route.fulfill(status=200, content_type='application/json', body=json.dumps(status))
    if url.startswith('/api/ix/dups'):
        return route.fulfill(status=200, content_type='application/json', body=json.dumps({'total': 0, 'clusters': [], 'spareSize': 0}))
    return route.fulfill(status=200, content_type='application/json', body='{}')


with sync_playwright() as p:
    b = p.chromium.launch()
    page = b.new_page(viewport={'width': 1200, 'height': 900})
    errors = []
    page.on('pageerror', lambda e: errors.append(str(e)))
    page.route(ORIGIN + '/**', handle)
    page.goto(ORIGIN + '/ix')
    btn = page.locator('[data-act="dl-loss"]')
    btn.wait_for()
    assert 'forecast' in btn.inner_text(), btn.inner_text()
    btn.click()
    page.locator('text=Forecast to the deletion date').wait_for()
    card = page.locator('.card', has_text='Before the deadline').inner_text()
    for want in ["At the last 24 hours' speed", "If Telegram's speed limit stays on", '38 of 40 queued file(s) finish', '30 of 40',
                 'probable copies held back', '1 GB or more', 'Ranked group', 'place 1', 'Invented Ranked',
                 'About 5 file(s)', 'Only groups already added count', 'Not counted: 1 file(s) paused']:
        assert want in card, (want, card)
    assert 'all' in page.locator('tr', has_text='Invented Ranked').first.inner_text()
    # The loss list still shows below.
    assert page.locator('tr', has_text='all read').count() >= 1
    assert not errors, errors
    if os.environ.get('IX_SHOT'):  # a picture of the card, for a look
        page.locator('.card', has_text='Before the deadline').screenshot(path=os.environ['IX_SHOT'])
    b.close()
print('archive forecast: ok')
