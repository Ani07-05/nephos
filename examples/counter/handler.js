async function handle(req) {
  if (req.method === 'POST') {
    const n = (kv.get('visits') || 0) + 1;
    kv.set('visits', n);
    kv.set('last', req.user);
    return { visits: n, last: req.user };
  }
  return { visits: kv.get('visits') || 0, last: kv.get('last') || null };
}
