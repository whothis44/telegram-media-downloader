"""Browser regression check: python3 tests/ui/archive_layout.py (requires Playwright Chromium).

ix.12.3 gives each Archive control one place. For every moved control: it is in
its new tab and in no other, and saving it sends the same setting key as before,
so saved settings and the dashboard keep working. Also: a setting changed and
not saved survives the page's redraws, the staging folder picker fills the
folder, "Install updates automatically" needs a launcher, the Rescan rank links
to Priorities, and finishing groups is done in Rescan only. Runs the real
ix.html with the API answered from invented data.
"""
import json
from pathlib import Path
from playwright.sync_api import sync_playwright

root = Path(__file__).resolve().parents[2]
html = (root / 'core-service/internal/app/ixassets/ix.html').read_text(encoding='utf-8')
ORIGIN = 'http://archive.test'

settings = {'pace': 'normal', 'quietStart': '', 'quietEnd': '', 'pauseOnFreeze': False, 'deadlineOrder': 'auto',
            'downloadPacing': True, 'autoRetry': True, 'substExact': True, 'holdLikely': True, 'holdForever': False,
            'fixityEnabled': True, 'lowqFirst': True, 'lowqKeep': False, 'bigAtOnce': 2, 'stageDir': '', 'stageCapGb': 20,
            'autoUpdate': False, 'confirmedGoEarly': False, 'copyThumbsOff': False, 'previewsAll': 'auto',
            'exportEnabled': True, 'exportTime': '03:30', 'exportDir': ''}
group = {'id': 'g1', 'name': 'Invented Chat', 'dir': 'g1', 'configured': True, 'messages': 10, 'deleted': 0, 'gone': 0,
         'tgCount': 10, 'lastWalk': 1, 'filesToGo': 0, 'files': 3, 'finishedAt': 0}
update = {'current': '1.0.0', 'latest': '1.0.0', 'newer': False, 'draining': False, 'restarts': False}
status = {'version': '1.0.0', 'groups': [group], 'queue': {'items': [{'groupId': 'g1', 'name': 'Invented Chat', 'state': 'waiting', 'priority': False}]},
          'queueLeft': {}, 'dlState': {}, 'settings': settings, 'account': {}, 'export': {}, 'freeze': {},
          'held': {'held': 3, 'subst': 2}, 'dedup': {}, 'copies': {'queue': {}, 'sampled': {'checked': 4}}, 'downloads': {}, 'dups': {},
          'readonly': {}, 'fixity': {'checked': 5, 'files': 9},
          'staging': {'suggest': 'E:\\data', 'drives': [{'root': 'C:\\', 'free': 50 << 30, 'total': 500 << 30}, {'root': 'E:\\', 'free': 900 << 30, 'total': 2000 << 30}]},
          'update': update}
prio = {'policy': 'order', 'known': True, 'distinct': {}, 'groups': [{'groupId': 'g1', 'name': 'Invented Chat', 'rank': 0, 'place': 0, 'scanned': True,
        'total': {}, 'saved': {}, 'shared': {}, 'probable': {}, 'needed': {}, 'neededAhead': {}, 'waiting': 'not ranked'},
        {'groupId': 'g2', 'name': 'Invented Ranked', 'rank': 1, 'place': 1, 'scanned': True, 'notQueued': True,
        'total': {}, 'saved': {}, 'shared': {}, 'probable': {}, 'needed': {'files': 7}, 'neededAhead': {}, 'waiting': 'files missing but not queued: add it to the rescan'}]}
saves = []
added = []


def handle(route, request):
    url = request.url.replace(ORIGIN, '')
    if url == '/ix' or url.startswith('/ix#'):
        return route.fulfill(status=200, content_type='text/html', body=html)
    if url.startswith('/api/ix/settings') and request.method == 'POST':
        saves.append(json.loads(request.post_data))
        return route.fulfill(status=200, content_type='application/json', body='{"success":true}')
    if url.startswith('/api/ix/queue/add') and request.method == 'POST':
        added.append(json.loads(request.post_data))
        return route.fulfill(status=200, content_type='application/json', body='{"success":true}')
    if url.startswith('/api/ix/priorities'):
        return route.fulfill(status=200, content_type='application/json', body=json.dumps(prio))
    if url.startswith('/api/ix/status'):
        return route.fulfill(status=200, content_type='application/json', body=json.dumps(status))
    if url.startswith('/api/ix/dups'):
        return route.fulfill(status=200, content_type='application/json', body=json.dumps({'total': 0, 'clusters': [], 'spareSize': 0}))
    return route.fulfill(status=200, content_type='application/json', body='{}')


# (setting key, how it is marked, tab it lives in now)
MOVED = [
    ('pace', 'data-set-select', 'rescan'),
    ('quietStart', 'data-set-text', 'rescan'),
    ('pauseOnFreeze', 'data-set', 'rescan'),
    ('deadlineOrder', 'data-set-select', 'priorities'),
    ('lowqFirst', 'data-set', 'priorities'),
    ('lowqKeep', 'data-set', 'priorities'),
    ('bigAtOnce', 'data-set-text', 'priorities'),
    ('downloadPacing', 'data-set', 'settings'),
    ('autoRetry', 'data-set', 'settings'),
    ('stageDir', 'data-set-text', 'settings'),
    ('stageCapGb', 'data-set-text', 'settings'),
    ('autoUpdate', 'data-set', 'settings'),
    ('substExact', 'data-set', 'duplicates'),
    ('holdLikely', 'data-set', 'duplicates'),
    ('holdForever', 'data-set', 'duplicates'),
    ('skipConfirmed', 'data-set', 'duplicates'),
    ('copyThumbs', 'data-set', 'duplicates'),
    ('previewsAll', 'data-set-select', 'duplicates'),
    ('fixityEnabled', 'data-set', 'overview'),
]
TABS = ['overview', 'priorities', 'rescan', 'duplicates', 'settings']

with sync_playwright() as p:
    browser = p.chromium.launch()
    page = browser.new_page(viewport={'width': 1200, 'height': 900})
    errors = []
    page.on('pageerror', lambda e: errors.append(str(e)))
    page.on('dialog', lambda d: d.accept())
    page.route(ORIGIN + '/**', handle)
    page.goto(ORIGIN + '/ix#overview')
    page.wait_for_selector('.tabs')

    def go(tab):
        page.click(f'.tabs [data-key="{tab}"]')
        page.wait_for_timeout(250)

    # Each moved control is in its tab, and in no other.
    found = {}
    for tab in TABS:
        go(tab)
        for key, attr, _ in MOVED:
            if page.locator(f'[{attr}="{key}"]').count():
                found.setdefault(key, []).append(tab)
    for key, _, tab in MOVED:
        assert found.get(key) == [tab], (key, found.get(key), tab)
    assert page.locator('h2:text("Account safety")').count() == 0
    assert 'Copies & duplicates' in page.inner_text('.tabs')

    # Saving a control in its new place sends the key it always had.
    def save_in(tab, key, attr, change):
        go(tab)
        el = page.locator(f'[{attr}="{key}"]').first
        change(el)
        card = el.locator('xpath=ancestor::div[contains(@class,"card")][1]')
        before = len(saves)
        card.locator('[data-act="set-save"]').click()
        page.wait_for_timeout(250)
        assert len(saves) == before + 1, (key, saves[before:])
        return saves[-1]

    assert save_in('rescan', 'pace', 'data-set-select', lambda e: e.select_option('gentle'))['pace'] == 'gentle'
    assert save_in('rescan', 'pauseOnFreeze', 'data-set', lambda e: e.check())['pauseOnFreeze'] is True
    assert save_in('priorities', 'deadlineOrder', 'data-set-select', lambda e: e.select_option('off'))['deadlineOrder'] == 'off'
    assert save_in('priorities', 'bigAtOnce', 'data-set-text', lambda e: e.fill('3'))['bigAtOnce'] == '3'
    assert save_in('settings', 'autoRetry', 'data-set', lambda e: e.uncheck())['autoRetry'] is False
    assert save_in('duplicates', 'holdLikely', 'data-set', lambda e: e.uncheck())['holdLikely'] is False
    assert save_in('duplicates', 'skipConfirmed', 'data-set', lambda e: e.uncheck())['skipConfirmed'] is False
    assert save_in('overview', 'fixityEnabled', 'data-set', lambda e: e.uncheck())['fixityEnabled'] is False

    # A change not saved yet survives the redraws (Overview and Rescan redraw every few seconds).
    go('rescan')
    page.fill('[data-set-text="quietStart"]', '22:00')
    go('rescan')  # a redraw, as the page's own refresh does
    page.wait_for_timeout(3500)  # and the Rescan tab's refresh
    assert page.input_value('[data-set-text="quietStart"]') == '22:00', 'a redraw undid an unsaved change'

    # The staging picker: a drive fills the folder; another folder shows the box.
    go('settings')
    assert page.locator('[data-set-text="stageDir"]').is_hidden()
    labels = page.locator('[data-stage-pick] option').all_inner_texts()
    assert any('suggested' in l for l in labels) and any('Drive C:\\' in l and 'free' in l for l in labels), labels
    page.select_option('[data-stage-pick]', 'C:\\')
    assert page.input_value('[data-set-text="stageDir"]') == 'C:\\'
    page.select_option('[data-stage-pick]', '__other')
    assert page.locator('[data-set-text="stageDir"]').is_visible()
    page.fill('[data-set-text="stageDir"]', 'D:\\Invented staging')
    go('settings')  # a redraw: the typed folder stays, and so does its box
    assert page.locator('[data-set-text="stageDir"]').is_visible()
    assert page.input_value('[data-set-text="stageDir"]') == 'D:\\Invented staging'
    card = page.locator('[data-stage-pick]').locator('xpath=ancestor::div[contains(@class,"card")][1]')
    card.locator('[data-act="set-save"]').click()
    page.wait_for_timeout(250)
    assert saves[-1]['stageDir'] == 'D:\\Invented staging', saves[-1]
    assert 'What it does' in card.inner_text()

    # Install updates automatically needs a launcher that loops.
    assert page.locator('[data-set="autoUpdate"]').is_disabled()
    assert 'TGDL_LAUNCHER_RESTARTS=1' in page.inner_text('#app')
    update['restarts'] = True
    page.reload()
    go('settings')
    assert page.locator('[data-set="autoUpdate"]').is_enabled()
    assert 'TGDL_LAUNCHER_RESTARTS=1' not in page.inner_text('#app')

    # Finishing groups: Rescan only; the Overview shows the state.
    go('overview')
    assert page.locator('[data-act="g-finish"],[data-act="g-open"],[data-act="g-finish-complete"]').count() == 0
    go('rescan')
    assert page.locator('[data-act="p-finish"]').count() == 1 and page.locator('[data-act="g-finish-complete"]').count() == 1

    # The Rescan list's rank: a link to Priorities, not a second ranking.
    assert page.locator('[data-act="q-priority"]').count() == 0
    page.click('[data-act="go-prio"]')
    page.wait_for_timeout(300)
    assert 'on' in page.get_attribute('.tabs [data-key="priorities"]', 'class')
    assert page.input_value('[data-in="prioSearch"]') == 'Invented Chat'

    # Ranked groups whose files nothing will queue: one click adds them to the
    # rescan, keeping their rank and not starting a paused rescan.
    assert '7 file(s) still needed' in page.inner_text('#app')
    page.click('[data-act="pr-rescan-missing"]')
    page.wait_for_timeout(300)
    assert added and added[-1]['groups'] == [{'id': 'g2', 'name': 'Invented Ranked'}], added
    assert added[-1]['start'] is False and added[-1]['priority'] is False, added[-1]

    # A phone: no sideways scroll in any tab.
    page.set_viewport_size({'width': 375, 'height': 800})
    for tab in TABS:
        go(tab)
        over = page.evaluate('document.documentElement.scrollWidth - document.documentElement.clientWidth')
        assert over <= 0, f'{tab}: {over}px of sideways scroll on a phone'

    assert not errors, errors
    print('archive layout: ok')
    browser.close()
