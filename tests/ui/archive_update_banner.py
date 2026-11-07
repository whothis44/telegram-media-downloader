"""Browser regression check: python3 tests/ui/archive_update_banner.py (requires Playwright Chromium).

The Archive page's update banner: offered when a newer release exists, waiting
with what is left, restarting by itself or asking a person when no launcher would
bring the app back. Runs the real ix.html with the API answered from invented data.
"""
import json
from pathlib import Path
from playwright.sync_api import sync_playwright

root = Path(__file__).resolve().parents[2]
html = (root / 'core-service/internal/app/ixassets/ix.html').read_text(encoding='utf-8')
ORIGIN = 'http://archive.test'
update = {'current': '1.0.0', 'latest': '2.0.0', 'newer': True, 'draining': False, 'files': 0, 'bytesLeft': 0, 'ready': False, 'restarts': True, 'exiting': False}
status = {'version': '1.0.0', 'groups': [], 'queue': {'items': []}, 'queueLeft': {}, 'dlState': {}, 'settings': {}, 'account': {},
          'export': {}, 'freeze': {}, 'held': {}, 'dedup': {}, 'copies': {}, 'downloads': {}, 'dups': {}, 'readonly': {}, 'update': update}
actions = []


def handle(route, request):
    url = request.url.replace(ORIGIN, '')
    if url == '/ix' or url.startswith('/ix#'):
        return route.fulfill(status=200, content_type='text/html', body=html)
    if url.startswith('/api/ix/update') and request.method == 'POST':
        action = json.loads(request.post_data)['action']
        actions.append(action)
        if action == 'start':
            update.update(draining=True, files=3, bytesLeft=2 << 30)
        elif action == 'cancel':
            update.update(draining=False, files=0, bytesLeft=0, ready=False)
        elif action == 'now':
            update.update(exiting=True)
        return route.fulfill(status=200, content_type='application/json', body=json.dumps(update))
    if url.startswith('/api/ix/status'):
        return route.fulfill(status=200, content_type='application/json', body=json.dumps(status))
    return route.fulfill(status=200, content_type='application/json', body='{}')


with sync_playwright() as p:
    browser = p.chromium.launch()
    page = browser.new_page(viewport={'width': 1100, 'height': 700})
    errors = []
    page.on('pageerror', lambda e: errors.append(str(e)))
    page.route(ORIGIN + '/**', handle)
    page.goto(ORIGIN + '/ix#overview')

    # Offered, and it says what it will not lose.
    page.wait_for_selector('[data-act="u-start"]')
    assert 'Version 2.0.0 is out' in page.inner_text('#app')

    # Waiting: what is left, and the two ways out.
    page.click('[data-act="u-start"]')
    page.wait_for_selector('[data-act="u-cancel"]')
    body = page.inner_text('#app')
    assert 'Waiting for 3 downloads in progress' in body and '2.00 GB left' in body, body
    assert 'stop at their next checkpoint' in body, body
    assert page.locator('[data-act="u-now"]').count() == 1

    # Going back.
    page.click('[data-act="u-cancel"]')
    page.wait_for_selector('[data-act="u-start"]')
    assert actions == ['start', 'cancel'], actions

    # Update now warns what starts over, and takes no for an answer.
    page.click('[data-act="u-start"]')
    page.wait_for_selector('[data-act="u-now"]')
    answers = []
    page.once('dialog', lambda d: (answers.append(d.message), d.dismiss()))
    page.click('[data-act="u-now"]')
    assert 'start over' in answers[0], answers
    assert 'now' not in actions, actions
    page.once('dialog', lambda d: d.accept())
    page.click('[data-act="u-now"]')
    page.wait_for_selector('text=Restarting for the update')
    assert actions[-1] == 'now', actions
    update.update(exiting=False)

    # No launcher to bring it back: no Update now, and it says what to do when done.
    update.update(restarts=False, draining=True, files=2, bytesLeft=1 << 20, ready=False)
    page.reload()
    page.wait_for_selector('[data-act="u-cancel"]')
    assert page.locator('[data-act="u-now"]').count() == 0
    update.update(files=0, bytesLeft=0, ready=True)
    page.reload()
    page.wait_for_selector('text=nothing will be lost')
    assert 'start.bat' in page.inner_text('#app')

    # Installing by itself (ix.12.3): it says so, and why it went back to downloading.
    update.update(restarts=True, newer=True, draining=True, files=1, bytesLeft=1 << 20, ready=False, autoNow=True, auto=True)
    page.reload()
    page.wait_for_selector('text=Installing version 2.0.0 by itself')
    update.update(draining=False, autoNow=False, autoNote='The automatic update waited 20m0s for the downloads in progress and went back to downloading; it tries again later.')
    page.reload()
    page.wait_for_selector('text=went back to downloading')
    assert 'Settings → Downloads can install' not in page.inner_text('#app'), 'offered a setting already on'
    update.update(auto=False, autoNote='')
    page.reload()
    page.wait_for_selector('[data-act="u-start"]')
    assert 'Settings → Downloads can install updates by themselves' in page.inner_text('#app')

    # Nothing new: no banner.
    update.update(newer=False, draining=False, ready=False)
    page.reload()
    page.wait_for_selector('.tabs')
    assert page.locator('[data-act="u-start"]').count() == 0
    assert not errors, errors
    print('archive update banner: ok')
    browser.close()
