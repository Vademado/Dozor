// Embed the exact standalone go2rtc page. It chooses its own playback mode,
// codecs and buffering. No media packets pass through the admin server.
export const STREAM_URL = 'http://127.0.0.1:1984/stream.html?src=luckfox';

export class GuardPlayer extends HTMLElement {
  start() {
    if (this.frame) return;
    const frame = document.createElement('iframe');
    frame.title = 'Прямая трансляция Luckfox — go2rtc';
    frame.allow = 'autoplay; fullscreen; picture-in-picture';
    frame.allowFullscreen = true;
    frame.src = STREAM_URL;
    this.frame = frame;
    this.appendChild(frame);
    // A loaded cross-origin page does not prove a video frame was decoded.
    // Playback status, errors and controls belong to the official player.
    this.dispatchEvent(new CustomEvent('player-state', {
      detail: {state: 'embedded', retries: 0}, bubbles: true,
    }));
  }

  dispose() {
    this.frame?.remove();
    this.frame = null;
  }

  disconnectedCallback() { this.dispose(); }
}

customElements.define('guard-player', GuardPlayer);
