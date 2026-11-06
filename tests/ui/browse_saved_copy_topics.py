"""Browser regression check: python3 tests/ui/browse_saved_copy_topics.py (requires Playwright Chromium).

ix.12.7, media browser:
1. A file not downloaded whose same file is saved from another post is tagged
   "copy saved", and its lightbox opens that saved copy and links to its post.
2. A forum chat shows a topic menu (its topics, most posts first); choosing a
   topic asks the feed for that topic only; a chat without topics hides it.
Runs the real browse.html with the API answered from invented data.
"""
import json
from pathlib import Path
from urllib.parse import urlparse, parse_qs
from playwright.sync_api import sync_playwright

root = Path(__file__).resolve().parents[2]
html = (root / 'core-service/internal/app/ixassets/browse.html').read_text(encoding='utf-8')
ORIGIN = 'http://archive.test'

saved = {'id': 7, 'key': 'h:aa', 'type': 'photo', 'status': 'ok', 'group': 'g2', 'msg': 5, 'name': 'saved-invented.jpg',
         'thumb': '/api/thumbs/7', 'src': '/files/g2/saved-invented.jpg?inline=1', 'copies': 2}
missing = {'type': 'photo', 'status': 'missing', 'group': 'f', 'msg': 3, 'name': 'missing-invented.jpg',
           'thumb': '/api/ix/preview?group=f&msg=3', 'copies': 2, 'like': saved}
info = {'built': 1, 'building': False, 'posts': 3, 'sets': 0, 'families': 3, 'repostPosts': 0, 'mergedAlbums': 0, 'held': 0, 'missing': 1,
        'media': {'done': 1, 'total': 1}, 'videos': {'done': 0, 'total': 0}, 'ml': {}, 'collections': [], 'categories': ['Invented'],
        'groups': [{'id': 'f', 'name': 'Invented Forum', 'category': 'Invented', 'posts': 2, 'items': 2,
                    'topics': [{'id': 7, 'name': 'Invented Topic', 'posts': 1}, {'id': 1, 'name': 'General', 'posts': 1}]},
                   {'id': 'g2', 'name': 'Invented Plain', 'category': 'Invented', 'posts': 1, 'items': 1}]}
card = {'kind': 'family', 'id': 0, 'date': 1700000000, 'group': 'f', 'groupName': 'Invented Forum', 'msg': 3, 'items': [missing], 'family': 0, 'posts': 1}
item = {'item': missing, 'post': {'id': 0, 'group': 'f', 'groupName': 'Invented Forum', 'msg': 3, 'date': 1700000000, 'role': 'original', 'family': 0, 'famPosts': 1, 'items': [missing]},
        'copies': [dict(missing, groupName='Invented Forum', date=1700000000, family=0, role='original'),
                   dict(saved, groupName='Invented Plain', date=1600000000, canon=True, family=1, role='original')],
        'generic': False, 'related': []}
feeds = []


def handle(route, request):
    u = urlparse(request.url)
    path, q = u.path, parse_qs(u.query)
    if path == '/browse':
        return route.fulfill(status=200, content_type='text/html', body=html)
    if path == '/api/ix/browse/info':
        return route.fulfill(status=200, content_type='application/json', body=json.dumps(info))
    if path == '/api/ix/browse/feed':
        feeds.append({k: v[0] for k, v in q.items()})
        return route.fulfill(status=200, content_type='application/json', body=json.dumps({'cards': [card], 'total': 1, 'next': 0}))
    if path == '/api/ix/browse/item':
        body = item if q.get('group', [''])[0] == 'f' or not q.get('id') else dict(item, item=saved, copies=item['copies'])
        return route.fulfill(status=200, content_type='application/json', body=json.dumps(body))
    if path.startswith('/api/thumbs') or path.startswith('/files') or path.startswith('/api/ix/preview'):
        return route.fulfill(status=200, content_type='image/gif', body=b'GIF89a\x01\x00\x01\x00\x00\x00\x00;')
    return route.fulfill(status=200, content_type='application/json', body='{}')


with sync_playwright() as p:
    browser = p.chromium.launch()
    page = browser.new_page(viewport={'width': 1200, 'height': 900})
    errors = []
    page.on('pageerror', lambda e: errors.append(str(e)))
    page.route(ORIGIN + '/**', handle)
    page.goto(ORIGIN + '/browse')
    page.wait_for_selector('.tile')

    # 1. The missing file says its copy is saved, and its lightbox reaches it.
    assert page.locator('.tile .lab.miss').inner_text() == 'copy saved', page.locator('.tile .lab.miss').inner_text()
    page.locator('.tile').first.click()
    page.wait_for_selector('#lb-saved')
    assert 'The same file, saved' in page.locator('#lb-saved').inner_text()
    assert page.locator('#lb-saved [data-open-chat]').get_attribute('data-open-chat') == 'g2:5'
    assert page.locator('#lb-info [data-open-copy]').count() == 1, 'one other copy can be opened'
    assert page.locator('#lb-info [data-keeper]').count() == 0, 'a missing file cannot choose a keeper'
    page.locator('#lb-saved [data-open-saved]').click()
    page.wait_for_function("document.querySelector('#dlg-title').textContent === 'saved-invented.jpg'")
    assert page.locator('#dlg .view img').get_attribute('src') == saved['src'], 'the saved copy itself is shown'

    # A copy listed under "copies" opens too.
    page.locator('#dlg-close').click()
    page.locator('.tile').first.click()
    page.wait_for_selector('#lb-info [data-open-copy]')
    page.locator('#lb-info [data-open-copy]').click()
    page.wait_for_function("document.querySelector('#dlg-title').textContent === 'saved-invented.jpg'")
    page.locator('#dlg-close').click()

    # 2. Topics: hidden for everything, shown for the forum, gone for a plain chat.
    assert page.locator('#topic').is_hidden()
    page.locator('#side [data-group="f"]').click()
    page.wait_for_function("!document.querySelector('#topic').hidden")
    opts = page.locator('#topic option').all_inner_texts()
    assert opts == ['All topics', 'Invented Topic (1)', 'General (1)'], opts
    assert feeds[-1].get('group') == 'f' and 'topic' not in feeds[-1], feeds[-1]
    n = len(feeds)
    page.select_option('#topic', '7')
    page.wait_for_function(f"window.__n = {n}, true")
    page.wait_for_timeout(200)
    assert feeds[-1].get('topic') == '7' and feeds[-1].get('group') == 'f', feeds[-1]
    page.select_option('#topic', '1')
    page.wait_for_timeout(200)
    assert feeds[-1].get('topic') == '1', feeds[-1]
    page.locator('#side [data-group="g2"]').click()
    page.wait_for_timeout(200)
    assert page.locator('#topic').is_hidden(), 'a chat without topics shows no topic menu'
    assert feeds[-1].get('group') == 'g2' and 'topic' not in feeds[-1], feeds[-1]
    page.locator('#side [data-all]').click()
    page.wait_for_timeout(200)
    assert 'topic' not in feeds[-1] and 'group' not in feeds[-1], feeds[-1]

    assert not errors, errors
    browser.close()
print('browse saved copy and topics: ok')
