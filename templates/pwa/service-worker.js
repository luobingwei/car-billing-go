/* 用车账单系统 PWA Service Worker
   策略：网络优先 —— 在线永远走网络（数据实时），断网时才用缓存兜底。
   /api/ 请求永不缓存（账单、费率都是动态数据）。 */
const CACHE = 'car-billing-v1';
const CORE = ['/', '/admin', '/pwa/manifest.json'];

self.addEventListener('install', e => {
  e.waitUntil(
    caches.open(CACHE).then(c => c.addAll(CORE)).then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', e => {
  e.waitUntil(
    caches.keys()
      .then(keys => Promise.all(keys.filter(k => k !== CACHE).map(k => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', e => {
  const req = e.request;
  if (req.method !== 'GET') return;            // POST 等一律走网络
  const url = new URL(req.url);
  if (url.origin !== location.origin) return;  // 跨域（Memos 等）不干预
  if (url.pathname.startsWith('/api/')) return; // 动态数据永不缓存
  // 网络优先，失败回退缓存
  e.respondWith(
    fetch(req)
      .then(res => {
        const copy = res.clone();
        caches.open(CACHE).then(c => c.put(req, copy)).catch(() => {});
        return res;
      })
      .catch(() => caches.match(req))
  );
});
