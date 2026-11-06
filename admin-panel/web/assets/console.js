import './vendor/xterm/xterm.js';
import './vendor/xterm/addon-fit.js';

const $ = id => document.getElementById(id);
const encoder = new TextEncoder();

// SSH credentials live only in the pending connection attempt. No password or
// terminal contents are written to settings, browser storage or the journal.
export class CameraConsole {
  constructor() {
    this.socket = null;
    this.pending = null;
    this.terminal = null;
    this.visible = false;
    this.state = 'disconnected';
    this.startedAt = 0;
    this.defaultLoaded = false;
    this.hostEdited = false;
    $('consoleForm').addEventListener('submit', event => {
      event.preventDefault();
      this.connect();
    });
    $('consoleHost').addEventListener('input', () => { this.hostEdited = true; });
    $('consoleDisconnect').addEventListener('click', () => this.disconnect());
    $('consoleTrust').addEventListener('click', () => {
      if (this.pending && this.fingerprint) {
        $('consoleHostKey').hidden = true;
        this.openSocket({...this.pending, trust_fingerprint: this.fingerprint});
      }
    });
    $('consoleCancelTrust').addEventListener('click', () => this.disconnect());
    $('consoleClear').addEventListener('click', () => { this.terminal?.clear(); this.terminal?.focus(); });
    $('consoleInterrupt').addEventListener('click', () => this.input('\x03'));
    $('consoleFullscreen').addEventListener('click', () => {
      const action = document.fullscreenElement ? document.exitFullscreen() : $('consoleCard').requestFullscreen();
      action.catch(() => this.notice('Браузер не разрешил полный экран.'));
    });
    this.resizeObserver = new ResizeObserver(() => this.fit());
    this.resizeObserver.observe($('consoleViewport'));
    window.addEventListener('beforeunload', () => this.disconnect(false));
    document.addEventListener('fullscreenchange', () => requestAnimationFrame(() => this.fit()));
    this.clock = setInterval(() => {
      const elapsed = this.startedAt ? Math.floor((Date.now() - this.startedAt) / 1000) : 0;
      $('consoleDuration').textContent = this.startedAt ? `${Math.floor(elapsed / 60).toString().padStart(2, '0')}:${(elapsed % 60).toString().padStart(2, '0')}` : '—';
    }, 1000);
    this.setState('disconnected', 'Введите пароль камеры и нажмите «Подключиться».');
  }

  setDefaultHost(host) {
    if (!this.hostEdited && this.state === 'disconnected') $('consoleHost').value = host;
  }

  setVisible(visible) {
    this.visible = visible;
    if (!visible) return;
    this.ensureTerminal();
    requestAnimationFrame(() => this.fit());
    if (!this.defaultLoaded) {
      this.defaultLoaded = true;
      fetch('/api/console/config').then(response => {
        if (!response.ok) throw new Error();
        return response.json();
      }).then(config => {
        this.setDefaultHost(config.host);
        if (!config.enabled) this.setState('error', 'Перезапустите обновлённую сборку сервера: SSH-консоль недоступна.');
      }).catch(() => {
        this.defaultLoaded = false;
        this.notice('Не удалось получить настройки консоли. Проверьте связь с панелью.');
      });
    }
  }

  ensureTerminal() {
    if (this.terminal) return;
    this.terminal = new globalThis.Terminal({
      cursorBlink: true, cursorStyle: 'bar', fontFamily: 'Consolas, "Cascadia Mono", monospace',
      fontSize: 14, lineHeight: 1.25, scrollback: 2000, disableStdin: true,
      theme: {background: '#10272c', foreground: '#e0eae3', cursor: '#d7be7e', selectionBackground: '#5c817d70', black: '#172d32', red: '#ef8b7b', green: '#91c9a6', yellow: '#dec484', blue: '#8fbbd0', magenta: '#c6a6d2', cyan: '#8fcaca', white: '#e0eae3'},
    });
    this.fitAddon = new globalThis.FitAddon.FitAddon();
    this.terminal.loadAddon(this.fitAddon);
    this.terminal.open($('consoleViewport'));
    this.terminal.writeln('\x1b[1;33mДОЗОР · Консоль камеры\x1b[0m\r\n');
    this.terminal.writeln('SSH-терминал появится здесь после подключения.\r\n');
    this.terminal.onData(data => this.input(data));
    this.terminal.onResize(({cols, rows}) => {
      $('consoleSize').textContent = `${cols} × ${rows}`;
      this.send({type: 'resize', cols, rows});
    });
  }

  fit() {
    if (!this.visible || !this.terminal || $('consoleViewport').clientWidth < 80) return;
    this.fitAddon.fit();
  }

  notice(message) { $('consoleMessage').textContent = message; }

  setState(state, message = '') {
    this.state = state;
    const busy = ['connecting', 'connected', 'host_key'].includes(state);
    const connected = state === 'connected';
    const names = {disconnected: 'Не подключено', connecting: 'Подключение…', connected: 'SSH подключён', host_key: 'Проверка ключа', error: 'Ошибка подключения', closed: 'Сеанс завершён'};
    $('consoleState').textContent = names[state] || names.disconnected;
    $('consoleStateDot').className = `status-dot ${connected ? 'green' : 'warn'}`;
    $('consoleNavDot').hidden = !connected;
    $('consoleConnect').disabled = busy;
    $('consoleConnect').querySelector('span').textContent = state === 'connecting' ? 'Подключение…' : 'Подключиться';
    $('consoleDisconnect').disabled = !busy;
    $('consoleInterrupt').disabled = !connected;
    for (const id of ['consoleHost', 'consolePort', 'consoleUser', 'consolePassword']) $(id).readOnly = busy;
    if (this.terminal) this.terminal.options.disableStdin = !connected;
    if (message) this.notice(message);
    if (!connected) this.startedAt = 0;
  }

  connect() {
    this.ensureTerminal();
    this.fit();
    const credentials = {
      host: $('consoleHost').value.trim(), port: Number($('consolePort').value),
      user: $('consoleUser').value.trim(), password: $('consolePassword').value,
      cols: this.terminal.cols, rows: this.terminal.rows,
    };
    if (!credentials.password) { $('consolePassword').focus(); return; }
    $('consolePassword').value = '';
    $('consoleHostKey').hidden = true;
    this.terminal.reset();
    this.openSocket(credentials);
  }

  openSocket(credentials) {
    this.socket?.close();
    this.clearPassword();
    this.pending = credentials;
    this.fingerprint = '';
    this.setState('connecting', `Подключаемся к ${credentials.host}:${credentials.port}…`);
    const endpoint = new URL('/api/console/ws', location.href);
    endpoint.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const socket = new WebSocket(endpoint);
    socket.binaryType = 'arraybuffer';
    this.socket = socket;
    const deadline = setTimeout(() => {
      if (this.socket === socket && this.state === 'connecting') {
        this.fail('Камера не ответила вовремя. Проверьте адрес и доступность SSH.');
        socket.close();
      }
    }, 18000);
    socket.onopen = () => {
      if (this.socket !== socket || !this.pending) return;
      socket.send(JSON.stringify({type: 'connect', ...this.pending}));
    };
    socket.onmessage = event => {
      if (this.socket !== socket) return;
      if (event.data instanceof ArrayBuffer) {
        const bytes = new Uint8Array(event.data);
        this.terminal.write(bytes, () => {
          if (this.socket === socket && socket.readyState === WebSocket.OPEN) socket.send(JSON.stringify({type: 'ack', bytes: bytes.byteLength}));
        });
        return;
      }
      let message;
      try { message = JSON.parse(event.data); } catch { return; }
      clearTimeout(deadline);
      if (message.type === 'connected') {
        this.clearPassword();
        this.startedAt = Date.now();
        this.setState('connected', 'Подключено. Команды выполняются на камере.');
        $('consoleTarget').textContent = `${credentials.user}@${credentials.host}:${credentials.port}`;
        credentials.password = '';
        this.fit(); this.terminal.focus();
      } else if (message.type === 'host_key') {
        this.fingerprint = message.fingerprint;
        $('consoleFingerprint').textContent = message.fingerprint;
        $('consoleKeyAlgorithm').textContent = message.algorithm;
        $('consoleHostKeyText').textContent = message.changed ? 'Ключ отличается от сохранённого. Сверьте ключ платы; автоматическое принятие отключено.' : 'Это первое подключение к этому SSH-ключу. Сверьте отпечаток с ключом платы и подтвердите его.';
        $('consoleTrust').hidden = message.changed;
        $('consoleHostKey').hidden = false;
        if (message.changed) this.clearPassword();
        this.setState('host_key', message.message);
      } else if (message.type === 'error') {
        this.fail(message.message);
      } else if (message.type === 'exit') {
        this.clearPassword();
        this.setState('closed', message.message);
        this.terminal.writeln(`\r\n\x1b[33m[${message.message}]\x1b[0m`);
      }
    };
    socket.onerror = () => {
      if (this.socket === socket) this.fail('Не удалось открыть соединение с сервером панели. Проверьте, что запущена обновлённая сборка.');
    };
    socket.onclose = () => {
      clearTimeout(deadline);
      if (this.socket !== socket) return;
      this.socket = null;
      if (this.state === 'host_key') return;
      this.clearPassword();
      if (this.state === 'connected') {
        this.setState('closed', 'Связь с камерой прервана. Для подключения снова введите пароль.');
        this.terminal.writeln('\r\n\x1b[33m[SSH-соединение закрыто]\x1b[0m');
      } else if (this.state === 'connecting') this.fail('Подключение закрыто до открытия терминала. Проверьте SSH камеры.');
    };
  }

  clearPassword() {
    if (this.pending) this.pending.password = '';
    this.pending = null;
    $('consolePassword').value = '';
  }

  fail(message) {
    this.clearPassword();
    this.setState('error', message || 'Не удалось открыть SSH-консоль.');
  }

  send(message) {
    if (this.state === 'connected' && this.socket?.readyState === WebSocket.OPEN) this.socket.send(JSON.stringify(message));
  }

  input(data) {
    if (this.state !== 'connected' || this.socket?.readyState !== WebSocket.OPEN) return;
    const bytes = encoder.encode(data);
    if (bytes.length > 65536 || this.socket.bufferedAmount + bytes.length > 131072) {
      this.notice('Ввод не отправлен: слишком большая вставка или камера временно не принимает данные.');
      return;
    }
    for (let offset = 0; offset < bytes.length; offset += 8192) this.socket.send(bytes.subarray(offset, offset + 8192));
    this.terminal.focus();
  }

  disconnect(showMessage = true) {
    this.clearPassword();
    const socket = this.socket;
    this.socket = null;
    if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify({type: 'disconnect'}));
    socket?.close();
    $('consoleHostKey').hidden = true;
    this.setState('disconnected', 'Введите пароль камеры и нажмите «Подключиться».');
    if (showMessage && this.terminal) this.terminal.writeln('\r\n\x1b[33m[Отключено оператором]\x1b[0m');
  }
}
