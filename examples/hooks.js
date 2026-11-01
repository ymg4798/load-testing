// Functions used by login-flow.yaml.

// Called once before the test; the returned object becomes global variables.
export function setup(ctx) {
  const r = http.get(ctx.vars.baseUrl + "/fast");
  if (r.status !== 200) fail("target is not up: " + r.status);
  return { startedAt: Date.now() };
}

// Pick a random item from the list the previous step returned.
export function pickItem(ctx) {
  const items = ctx.last.json.items;
  if (!items || items.length === 0) return false; // fails the iteration
  ctx.vars.itemId = items[Math.floor(Math.random() * items.length)].id;
}
