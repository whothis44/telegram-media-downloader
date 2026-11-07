"""Browser regression check: python3 tests/ui/archive_rescan_ranking.py (requires Playwright Chromium).

ix.12.13, the Priorities tab and the rescan: groups in the rescan that are not
ranked are listed first under "Not ranked", marked, and one button puts them
at the end of the ranking; "Add it to the rescan" for a ranked group whose
files nothing queues says why when the server refuses (before, the reload
cleared the message at once). Runs the real ix.html with the API answered
from invented data.
"""
import json
from pathlib import Path
from playwright.sync_api import sync_playwright

root = Path(__file__).resolve().parents[2]
html = (root / 'core-service/internal/app/ixassets/ix.html').read_text(encoding='utf-8')
ORIGIN = 'http://archive.test'


def cover(files):
    return {'files': files, 'bytes': files << 20}


def group(gid, name, rank, needed=0, in_rescan=False, not_queued=False):
    return {'groupId': gid, 'name': name, 'rank': rank, 'place': rank, 'scanned': True,
            'total': cover(needed), 'saved': cover(0), 'shared': cover(0), 'probable': cover(0), 'needed': cover(needed),
            'neededAhead': cover(0), 'left': 0, 'apart': 0, 'waiting': '', 'inRescan': in_rescan, 'notQueued': not_queued}


state = {'policy': 'order', 'known': True, 'focus': '', 'rate': 0, 'rateDay': 0, 'rate3h': 0, 'until': 0,
         'distinct': {'saved': cover(0), 'probable': cover(0), 'needed': cover(0)}, 'groupRefs': 0,
         'groups': [group('a', 'Ranked One', 1), group('n', 'Ranked Missing', 2, needed=50, not_queued=True),
                    group('z', 'Big Unranked', 0, needed=900), group('y', 'Just Added', 0, in_rescan=True)]}
status = {'version': 'test', 'groups': [], 'queue': {'items': []}, 'queueLeft': {}, 'dlState': {}, 'settings': {}, 'account': {},
          'export': {}, 'freeze': {}, 'held': {}, 'dedup': {}, 'copies': {}, 'downloads': {}, 'dups': {}, 'readonly': {}}
posts = []


def handle(route, request):
    url = request.url.replace(ORIGIN, '')
    if url == '/ix' or url.startswith('/ix#'):
        return route.fulfill(status=200, content_type='text/html', body=html)
    if url.startswith('/api/ix/priorities'):
        if request.method == 'POST':
            body = json.loads(request.post_data)
            posts.append(body)
            if 'order' in body:
                by_id = {g['groupId']: g for g in state['groups']}
                for g in state['groups']:
                    g['rank'] = g['place'] = 0
                for i, gid in enumerate(body['order']):
                    by_id[gid]['rank'] = by_id[gid]['place'] = i + 1
            return route.fulfill(status=200, content_type='application/json', body='{"success":true}')
        return route.fulfill(status=200, content_type='application/json', body=json.dumps(state))
    if url.startswith('/api/ix/queue/add'):
        posts.append(json.loads(request.post_data))
        return route.fulfill(status=502, content_type='application/json', body='{"error":"could not update Ranked Missing: invented refusal"}')
    if url.startswith('/api/ix/status'):
        return route.fulfill(status=200, content_type='application/json', body=json.dumps(status))
    return route.fulfill(status=200, content_type='application/json', body='{}')


with sync_playwright() as p:
    browser = p.chromium.launch()
    page = browser.new_page(viewport={'width': 1200, 'height': 900})
    errors = []
    page.on('pageerror', lambda e: errors.append(str(e)))
    page.route(ORIGIN + '/**', handle)
    page.goto(ORIGIN + '/ix#priorities')
    page.wait_for_selector('[data-prio-row]')
    body = page.inner_text('#app')
    assert '1 group(s) in the rescan are not ranked' in body, body
    # Listed first under Not ranked, though it needs less than the other.
    others = page.eval_on_selector_all('[data-act="pr-add"]', 'b => b.map(x => x.getAttribute("data-id"))')
    assert others[:2] == ['y', 'z'], others
    assert 'in the rescan' in body, body

    # A refused add says why, and the message stays after the reload.
    page.click('[data-act="pr-rescan-missing"]')
    page.wait_for_function('document.querySelector("#app").innerText.includes("Could not add")')
    page.wait_for_timeout(300)
    assert 'invented refusal' in page.inner_text('#app'), page.inner_text('#app')
    assert posts[-1]['groups'] == [{'id': 'n', 'name': 'Ranked Missing'}], posts[-1]

    # One click ranks the rescan's groups at the end.
    page.click('[data-act="pr-rank-rescan"]')
    page.wait_for_selector('[data-prio-row="y"]')
    assert posts[-1]['order'] == ['a', 'n', 'y'], posts[-1]
    assert errors == [], errors
    browser.close()
print('ok')
