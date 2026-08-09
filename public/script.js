// ==========================================================================
// 1. SOCKET
// ==========================================================================
// Native WebSocket shim replacing socket.io (server is the airinput-tv Go host).
function createSocket() {
  const handlers = {};
  const api = {
    ws: null,
    connect() {
      const ws = new WebSocket(`ws://${location.host}/ws`);
      api.ws = ws;
      ws.onopen = () => fire('connect');
      ws.onmessage = (e) => {
        try {
          const msg = JSON.parse(e.data);
          fire(msg.event, msg.data);
        } catch (err) {
          console.error('Bad message from server:', err);
        }
      };
      ws.onclose = () => fire('disconnect');
      ws.onerror = () => fire('connect_error');
    },
    emit(event, data) {
      if (api.ws && api.ws.readyState === WebSocket.OPEN) {
        api.ws.send(JSON.stringify({ event, data }));
      }
    },
    on(event, fn) {
      (handlers[event] = handlers[event] || []).push(fn);
    },
  };
  function fire(event, data) {
    (handlers[event] || []).forEach((fn) => fn(data));
  }
  return api;
}
const socket = createSocket();

// ==========================================================================
// 2. DOM
// ==========================================================================
const joinScreen = document.getElementById('join-screen');
const usernameInput = document.getElementById('username-input');
const joinBtn = document.getElementById('join-btn');
const connStatus = document.getElementById('conn-status');
const connText = connStatus.querySelector('.conn-text');

const gamepadContainer = document.getElementById('gamepad-container');
const btnSettings = document.getElementById('btn-settings');
const settingsModal = document.getElementById('settings-modal');
const settingsBackdrop = document.getElementById('settings-backdrop');
const btnCloseSettings = document.getElementById('close-settings');
const btnDisconnect = document.getElementById('btn-disconnect');
const skinList = document.getElementById('skin-list');
const optHaptics = document.getElementById('opt-haptics');
const optFullscreen = document.getElementById('opt-fullscreen');
const toastEl = document.getElementById('toast');

let activeButtons = new Set();
let activeJoysticks = [];
let currentSkin = null;
let hasJoined = false;

// Preferences persist per device; the defaults match the checked state in HTML.
const prefs = {
  haptics: localStorage.getItem('airinput_haptics') !== '0',
  fullscreen: localStorage.getItem('airinput_fullscreen') !== '0',
};

// ==========================================================================
// 3. SMALL UI HELPERS
// ==========================================================================
let toastTimer;
function toast(message, isError = false) {
  toastEl.textContent = message;
  toastEl.classList.toggle('error', isError);
  toastEl.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => toastEl.classList.remove('show'), 3200);
}

function setConnState(state, text) {
  connStatus.dataset.state = state;
  connText.textContent = text;
}

function openSettings() {
  settingsModal.hidden = false;
  settingsBackdrop.hidden = false;
}

function closeSettings() {
  settingsModal.hidden = true;
  settingsBackdrop.hidden = true;
}

// ==========================================================================
// 4. SKINS
// ==========================================================================
// The picker is built from skins/skins.json, so dropping in a folder and adding
// an entry is all it takes to add a controller layout. See the README.
const FALLBACK_SKINS = [
  { id: 'xbox', name: 'Modern', blurb: 'Dual sticks, D-pad, ABXY' },
  { id: 'snes', name: 'Classic', blurb: 'D-pad and four face buttons' },
];

async function loadSkinRegistry() {
  try {
    const res = await fetch('skins/skins.json');
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const data = await res.json();
    if (!Array.isArray(data.skins) || !data.skins.length) throw new Error('empty registry');
    return data.skins;
  } catch (e) {
    // A broken registry should cost you the picker, not the controller.
    console.error('Could not read skins.json, using built-ins:', e);
    return FALLBACK_SKINS;
  }
}

function renderSkinPicker(skins) {
  skinList.innerHTML = '';
  skins.forEach((skin) => {
    const btn = document.createElement('button');
    btn.className = 'sel-btn';
    btn.type = 'button';
    btn.setAttribute('aria-pressed', String(skin.id === currentSkin));
    btn.dataset.skin = skin.id;
    const name = document.createElement('strong');
    name.textContent = skin.name || skin.id;
    btn.appendChild(name);
    if (skin.blurb) {
      const blurb = document.createElement('small');
      blurb.textContent = skin.blurb;
      btn.appendChild(blurb);
    }
    btn.addEventListener('click', () => loadSkin(skin.id));
    skinList.appendChild(btn);
  });
}

function markActiveSkin() {
  skinList.querySelectorAll('.sel-btn').forEach((b) => {
    b.setAttribute('aria-pressed', String(b.dataset.skin === currentSkin));
  });
}

async function loadSkin(skinName) {
  try {
    const response = await fetch(`skins/${skinName}/layout.html`);
    if (!response.ok) throw new Error('Skin not found');

    gamepadContainer.innerHTML = await response.text();

    const styleLink = document.getElementById('skin-style');
    styleLink.href = `skins/${skinName}/style.css`;

    currentSkin = skinName;
    localStorage.setItem('gamepad_skin', skinName);
    markActiveSkin();
    closeSettings();

    // Let the new layout lay out before nipplejs measures its zones.
    setTimeout(initJoysticks, 50);
  } catch (e) {
    console.error('Error loading skin:', e);
    toast(`Could not load the "${skinName}" layout`, true);
  }
}
window.loadSkin = loadSkin; // handy from the console when authoring a skin

// ==========================================================================
// 5. GAMEPAD INPUT
// ==========================================================================
function updateButton(btnName, state) {
  const cleanName = btnName.trim();
  socket.emit('input', { button: cleanName, state: state });

  const el = document.querySelector(`button[data-btn="${cleanName}"]`);
  if (el) el.classList.toggle('active', state === 1);

  if (state === 1 && prefs.haptics && navigator.vibrate) navigator.vibrate(18);
}

function scanGamePad(e) {
  if (
    e.target.closest('#btn-settings') ||
    e.target.closest('#settings-modal') ||
    e.target.closest('#settings-backdrop') ||
    e.target.closest('.stick-zone')
  ) {
    return;
  }

  if (e.type !== 'click') e.preventDefault();

  // Rebuild the pressed set from live touch points every event: this is what
  // makes sliding a thumb between buttons register as release + press.
  const buttonsBeingTouched = new Set();
  for (let i = 0; i < e.touches.length; i++) {
    const touch = e.touches[i];
    const element = document.elementFromPoint(touch.clientX, touch.clientY);
    if (element) {
      const btn = element.closest('button');
      if (btn) {
        const rawData = btn.dataset.btns || btn.dataset.btn;
        if (rawData) rawData.split(',').forEach((t) => buttonsBeingTouched.add(t.trim()));
      }
    }
  }

  activeButtons.forEach((btnName) => {
    if (!buttonsBeingTouched.has(btnName)) updateButton(btnName, 0);
  });
  buttonsBeingTouched.forEach((btnName) => {
    if (!activeButtons.has(btnName)) updateButton(btnName, 1);
  });

  activeButtons = buttonsBeingTouched;
}

function initJoysticks() {
  activeJoysticks.forEach((j) => j.destroy());
  activeJoysticks = [];

  const processJoystickData = (data) => {
    if (!data.vector) return { x: 0, y: 0 };
    let x = data.vector.x;
    let y = -data.vector.y;

    // The pad is rotated 90deg in portrait, so rotate the vector to match.
    if (window.innerHeight > window.innerWidth) {
      const tempX = x;
      x = y;
      y = -tempX;
    }
    return { x, y };
  };

  const createJoystick = (zoneId, onMove, onEnd) => {
    const zone = document.getElementById(zoneId);
    if (!zone) return; // skins without sticks simply omit the zone
    const joystick = nipplejs.create({
      zone,
      mode: 'static',
      position: { left: '50%', top: '50%' },
      color: 'white',
      size: 100,
    });
    joystick.on('move', (evt, data) => onMove(processJoystickData(data)));
    joystick.on('end', onEnd);
    activeJoysticks.push(joystick);
  };

  createJoystick(
    'stick-left-zone',
    ({ x, y }) => {
      socket.emit('axis', { axis: 'lx', value: x });
      socket.emit('axis', { axis: 'ly', value: y });
    },
    () => {
      socket.emit('axis', { axis: 'lx', value: 0 });
      socket.emit('axis', { axis: 'ly', value: 0 });
    }
  );

  createJoystick(
    'stick-right-zone',
    ({ x, y }) => {
      socket.emit('axis', { axis: 'rx', value: x });
      socket.emit('axis', { axis: 'ry', value: y });
    },
    () => {
      socket.emit('axis', { axis: 'rx', value: 0 });
      socket.emit('axis', { axis: 'ry', value: 0 });
    }
  );
}

// ==========================================================================
// 6. SESSION
// ==========================================================================
function goFullscreen() {
  if (!prefs.fullscreen || document.fullscreenElement) return;
  const el = document.documentElement;
  const req = el.requestFullscreen || el.webkitRequestFullscreen;
  if (req) Promise.resolve(req.call(el)).catch(() => { /* denied; not fatal */ });
}

async function initializeGamepad() {
  hasJoined = true;
  gamepadContainer.hidden = false;
  btnSettings.hidden = false;
  joinScreen.hidden = true;

  document.addEventListener('touchstart', scanGamePad, { passive: false });
  document.addEventListener('touchmove', scanGamePad, { passive: false });
  document.addEventListener('touchend', scanGamePad, { passive: false });
  document.addEventListener('touchcancel', scanGamePad, { passive: false });
  document.addEventListener('contextmenu', (e) => e.preventDefault());
  window.addEventListener('resize', () => setTimeout(initJoysticks, 200));

  // Fullscreen needs a user gesture, so it can only be asked for on first touch.
  const onFirstTouch = () => {
    goFullscreen();
    document.body.removeEventListener('touchstart', onFirstTouch);
  };
  document.body.addEventListener('touchstart', onFirstTouch);

  const skins = await loadSkinRegistry();
  const saved = localStorage.getItem('gamepad_skin');
  const wanted = skins.some((s) => s.id === saved) ? saved : skins[0].id;
  renderSkinPicker(skins);
  await loadSkin(wanted);
}

function returnToJoin(message, isError) {
  hasJoined = false;
  closeSettings();
  gamepadContainer.hidden = true;
  btnSettings.hidden = true;
  joinScreen.hidden = false;
  joinBtn.classList.remove('is-busy');
  joinBtn.disabled = false;
  if (message) toast(message, isError);
}

// ==========================================================================
// 7. BOOT
// ==========================================================================
document.addEventListener('DOMContentLoaded', () => {
  const savedUsername = localStorage.getItem('airinput_username');
  if (savedUsername) usernameInput.value = savedUsername;

  optHaptics.checked = prefs.haptics;
  optFullscreen.checked = prefs.fullscreen;

  setConnState('connecting', 'Connecting to TV…');
  socket.connect();

  const submitJoin = () => {
    const username = usernameInput.value.trim();
    if (!username) {
      toast('Enter a name first', true);
      usernameInput.focus();
      return;
    }
    if (!socket.ws || socket.ws.readyState !== WebSocket.OPEN) {
      toast('Not connected to the TV yet', true);
      return;
    }
    localStorage.setItem('airinput_username', username);
    joinBtn.classList.add('is-busy');
    joinBtn.disabled = true;
    socket.emit('register_player', { username });
  };

  joinBtn.addEventListener('click', submitJoin);
  usernameInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') submitJoin();
  });

  btnSettings.addEventListener('click', (e) => {
    e.stopPropagation();
    openSettings();
  });
  btnCloseSettings.addEventListener('click', closeSettings);
  settingsBackdrop.addEventListener('click', closeSettings);

  btnDisconnect.addEventListener('click', () => {
    if (socket.ws) socket.ws.close();
    returnToJoin('Disconnected');
  });

  optHaptics.addEventListener('change', () => {
    prefs.haptics = optHaptics.checked;
    localStorage.setItem('airinput_haptics', prefs.haptics ? '1' : '0');
  });

  optFullscreen.addEventListener('change', () => {
    prefs.fullscreen = optFullscreen.checked;
    localStorage.setItem('airinput_fullscreen', prefs.fullscreen ? '1' : '0');
    if (prefs.fullscreen) goFullscreen();
    else if (document.exitFullscreen && document.fullscreenElement) document.exitFullscreen();
  });

  socket.on('connect', () => setConnState('online', 'Connected to TV'));

  socket.on('connect_error', () => setConnState('offline', 'Cannot reach the TV'));

  socket.on('registration_success', () => initializeGamepad());

  socket.on('registration_failed', (message) => {
    joinBtn.classList.remove('is-busy');
    joinBtn.disabled = false;
    toast(message ? String(message) : 'The TV refused the connection', true);
  });

  socket.on('disconnect', () => {
    setConnState('offline', 'Disconnected — reload to reconnect');
    if (hasJoined) returnToJoin('Lost connection to the TV. Reload to reconnect.', true);
  });
});
