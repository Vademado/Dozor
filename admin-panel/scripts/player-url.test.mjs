import assert from 'node:assert/strict';
import test from 'node:test';

globalThis.HTMLElement = class {};
globalThis.customElements = {define() {}};

test('the player uses the browser-visible host instead of Docker DNS', async () => {
  const cases = [
    ['http://127.0.0.1:8080', 'http://127.0.0.1:1984/stream.html?src=luckfox'],
    ['http://localhost:8080', 'http://localhost:1984/stream.html?src=luckfox'],
    ['http://192.168.1.156:8080', 'http://192.168.1.156:1984/stream.html?src=luckfox'],
    ['http://[::1]:8080', 'http://[::1]:1984/stream.html?src=luckfox'],
    ['https://dozor.example:8080', 'https://dozor.example:1984/stream.html?src=luckfox'],
  ];
  for (const [index, [origin, expected]] of cases.entries()) {
    globalThis.window = {location: {origin}};
    const {STREAM_URL} = await import(`../web/assets/player.js?case=${index}`);
    assert.equal(STREAM_URL, expected);
  }
});
