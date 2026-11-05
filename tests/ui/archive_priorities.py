"""Browser regression check: python3 tests/ui/archive_priorities.py (requires Playwright Chromium).

The Archive page's Priorities tab: ranked groups in their order, moving and
removing one sends the whole list, a policy change reaches the server, and
probable copies are shown apart from what is saved. Runs the real ix.html with
the API answered from invented data.
"""
import json
import os
from pathlib import Path
from playwright.sync_api import sync_playwright

root = Path(__file__).resolve().parents[2]
html = (root / 'core-service/internal/app/ixassets/ix.html').read_text(encoding='utf-8')
ORIGIN = 'http://archive.test'


def cover(files, bytes_):
    return {'files': files, 'bytes': bytes_}


def group(gid, name, rank, place, saved=0, shared=0, probable=0, needed=0, scanned=True, waiting='', left=0):
    mb = 1 << 20
    return {
        'groupId': gid, 'name': name, 'rank': rank, 'place': place, 'scanned': scanned,
        'total': cover(saved + shared + probable + needed, (saved + shared + probable + needed) * mb),
        'saved': cover(saved, saved * mb), 'shared': cover(shared, shared * mb),
        'probable': cover(probable, probable * mb), 'needed': cover(needed, needed * mb),
        'neededAhead': cover(0, 0), 'left': left, 'apart': 0, 'waiting': waiting,
    }


state = {
    'policy': 'order', 'known': True, 'focus': 'Invented One', 'rate': 200000, 'rateDay': 2800000, 'rate3h': 200000, 'until': 0,
    'distinct': {'saved': cover(100, 100 << 20), 'probable': cover(7, 7 << 20), 'needed': cover(50, 50 << 20)},
    'groupRefs': 130,
    'groups': [
        group('a', 'Invented One', 1, 1, saved=40, shared=10, probable=7, needed=30, waiting='going first', left=30),
        group('b', 'Invented <b>Two</b>', 2, 2, saved=5, shared=25, needed=20, scanned=False, waiting='waiting for groups ranked above it'),
        group('c', 'Invented Three', 3, 3, saved=1, needed=2, waiting='waiting for groups ranked above it'),
        group('z', 'Not Ranked', 0, 0, saved=9, needed=4, waiting='not ranked: goes after every ranked group'),
    ],
}
status = {'version': 'test', 'groups': [], 'queue': {'items': []}, 'queueLeft': {}, 'dlState': {}, 'settings': {}, 'account': {},
          'export': {}, 'freeze': {}, 'held': {}, 'dedup': {}, 'copies': {}, 'downloads': {}, 'dups': {}, 'readonly': {}}
posts = []


def apply(body):
    posts.append(body)
    if 'policy' in body:
        state['policy'] = body['policy']
    if 'order' in body:
        by_id = {g['groupId']: g for g in state['groups']}
        for g in state['groups']:
            g['rank'] = g['place'] = 0
        for i, gid in enumerate(body['order']):
            by_id[gid]['rank'] = by_id[gid]['place'] = i + 1


def handle(route, request):
    url = request.url.replace(ORIGIN, '')
    if url == '/ix' or url.startswith('/ix#'):
        return route.fulfill(status=200, content_type='text/html', body=html)
    if url.startswith('/api/ix/priorities'):
        if request.method == 'POST':
            apply(json.loads(request.post_data))
        return route.fulfill(status=200, content_type='application/json', body=json.dumps(state))
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

    def ranked():
        return page.eval_on_selector_all('[data-prio-row]', 'rows => rows.map(r => r.getAttribute("data-prio-row"))')

    assert ranked() == ['a', 'b', 'c'], ranked()
    body = page.inner_text('#app')
    assert 'Going first now: Invented One' in body, body
    assert 'not fully read' in body
    # A name is text, never markup.
    assert page.locator('b >> text=Two').count() == 0 or '<b>Two</b>' in body, body
    # Probable copies have their own column; they are not added to Saved.
    row = page.locator('[data-prio-row="a"]').inner_text()
    assert '40' in row and '10' in row and '7' in row and '30' in row, row

    # Move down: the whole list goes to the server, and the page follows it.
    page.click('[data-prio-row="a"] [data-act="pr-move"][data-key="1"]')
    page.wait_for_function('document.querySelector("[data-prio-row]").getAttribute("data-prio-row") === "b"')
    assert posts[-1]['order'] == ['b', 'a', 'c'], posts[-1]
    assert ranked() == ['b', 'a', 'c'], ranked()

    # To the top.
    page.click('[data-prio-row="c"] [data-act="pr-top"]')
    page.wait_for_function('document.querySelector("[data-prio-row]").getAttribute("data-prio-row") === "c"')
    assert posts[-1]['order'] == ['c', 'b', 'a'], posts[-1]

    # Remove, then add.
    page.click('[data-prio-row="a"] [data-act="pr-remove"]')
    page.wait_for_function('!document.querySelector("[data-prio-row=\\"a\\"]")')
    assert posts[-1]['order'] == ['c', 'b'], posts[-1]
    page.click('[data-act="pr-add"][data-id="z"]')
    page.wait_for_selector('[data-prio-row="z"]')
    assert posts[-1]['order'] == ['c', 'b', 'z'], posts[-1]

    # Drag.
    page.drag_and_drop('[data-prio-row="z"]', '[data-prio-row="c"]', target_position={'x': 40, 'y': 2})
    page.wait_for_function('document.querySelector("[data-prio-row]").getAttribute("data-prio-row") === "z"')
    assert posts[-1]['order'] == ['z', 'c', 'b'], posts[-1]

    # Policy.
    page.check('[data-prio-policy="spread"]')
    page.wait_for_function('document.querySelector("[data-prio-policy=spread]").checked')
    assert any(b.get('policy') == 'spread' for b in posts), posts

    if os.environ.get('UI_SHOTS'):
        page.screenshot(path=os.path.join(os.environ['UI_SHOTS'], 'priorities.png'), full_page=True)

    # A phone: the page itself never scrolls sideways.
    page.set_viewport_size({'width': 375, 'height': 800})
    page.wait_for_timeout(100)
    overflow = page.evaluate('document.documentElement.scrollWidth - document.documentElement.clientWidth')
    assert overflow <= 0, f'{overflow}px of sideways scroll on a phone'
    if os.environ.get('UI_SHOTS'):
        page.screenshot(path=os.path.join(os.environ['UI_SHOTS'], 'priorities-phone.png'), full_page=True)

    assert not errors, errors
    print('archive priorities: ok')
    browser.close()
