// Class question board. Runs on the server for every request to /api/*.
// Storage: kv 'questions' is a list of { id, text, by, at, votes: [emails] }.

const MAX_LENGTH = 280;
const MAX_QUESTIONS = 200;

function nameOf(email) {
  return email === 'owner' ? 'host' : email.split('@')[0];
}

function view(questions, me) {
  return questions
    .map(q => ({
      id: q.id,
      text: q.text,
      by: nameOf(q.by),
      at: q.at,
      votes: q.votes.length,
      voted: q.votes.includes(me),
      mine: q.by === me,
    }))
    .sort((a, b) => b.votes - a.votes || b.at - a.at);
}

function fail(status, message) {
  return { status, body: { error: message } };
}

async function handle(req) {
  const me = req.user;
  const questions = kv.get('questions') || [];

  if (req.method === 'GET') {
    return { course: env.COURSE || 'Class', me: nameOf(me), questions: view(questions, me) };
  }
  if (!me) return fail(403, 'Sign in to take part.');

  const body = JSON.parse(req.body || '{}');

  if (req.path === '/ask') {
    const text = String(body.text || '').trim();
    if (!text) return fail(400, 'Write a question first.');
    if (text.length > MAX_LENGTH) return fail(400, `Keep it under ${MAX_LENGTH} characters.`);
    questions.push({ id: Date.now().toString(36) + Math.random().toString(36).slice(2, 6), text, by: me, at: Date.now(), votes: [me] });
    kv.set('questions', questions.slice(-MAX_QUESTIONS));
    return { ok: true };
  }

  if (req.path === '/vote') {
    const q = questions.find(x => x.id === body.id);
    if (!q) return fail(404, 'That question is gone.');
    q.votes = q.votes.includes(me) ? q.votes.filter(v => v !== me) : [...q.votes, me];
    kv.set('questions', questions);
    return { ok: true };
  }

  if (req.path === '/remove') {
    const q = questions.find(x => x.id === body.id);
    if (!q) return fail(404, 'That question is gone.');
    if (q.by !== me && me !== 'owner') return fail(403, 'Only the asker or the host can remove this.');
    kv.set('questions', questions.filter(x => x.id !== body.id));
    return { ok: true };
  }

  return fail(404, 'Unknown action.');
}
