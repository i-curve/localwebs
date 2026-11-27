<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, shallowRef, watch } from 'vue';
import { BlobReader, ZipWriter } from '@zip.js/zip.js';
import LanguagePicker from './LanguagePicker.vue';
import { tr } from './i18n';

const CHUNK = 256 * 1024;
const LIMIT = 100_000_000_000;
const TEXT_LIMIT = 1024 * 1024;
const ID = /^[0-9a-f]{32}$/;
const CODE = /^\d{6}$/;
const encoder = new TextEncoder();
const decoder = new TextDecoder();

type Signal = { kind: string; id: string; name?: string; size?: number | null; content?: 'text' };
type Transfer = { peer: string; id: string };
type Offer = Transfer & { name: string; size: number | null; content?: 'text'; token?: string };
type Incoming = Offer & { chunks: ArrayBuffer[]; received: number; timer?: number };
type Outgoing = Transfer & { abort: AbortController };
type Notice = { key: string; name?: string; peer?: string; text?: boolean };
type Sender = (write: (bytes: Uint8Array) => Promise<void>, abort: AbortSignal) => Promise<void>;

const ready = ref(false);
const room = ref('');
const input = ref('');
const roomError = ref('');
const changing = ref(true);
const joining = ref(false);
const peers = ref<string[]>([]);
const self = ref('');
const notice = ref<Notice>();
const failure = ref('');
const progress = ref<number>();
const fileInput = ref<HTMLInputElement>();
const folderInput = ref<HTMLInputElement>();
const offerDialog = ref<HTMLDialogElement>();
const clipboard = ref('');
const clipboardFiles = shallowRef<File[]>([]);
const selection = shallowRef<{ files: File[]; folder: boolean }>();
const receivedText = ref('');
const receivedFrom = ref('');
const copyMessage = ref('');
const offer = ref<Offer>();
const incoming = shallowRef<Incoming>();
const outgoing = shallowRef<Outgoing>();
const busy = computed(() => Boolean(offer.value || incoming.value || outgoing.value || receivedText.value));
const textDialogOpen = computed(() => incoming.value?.content === 'text' || Boolean(receivedText.value));
watch([offer, textDialogOpen], ([currentOffer, showText]) => {
  const dialog = offerDialog.value;
  if (!dialog) return;
  if ((currentOffer || showText) && !dialog.open) dialog.showModal();
  else if (!currentOffer && !showText && dialog.open) dialog.close();
}, { flush: 'post' });
let ws: WebSocket | undefined;
let reconnect: number | undefined;
let offerTimer: number | undefined;
let stopped = false;
let waiter: { kind: string; finish: (error?: Error) => void } | undefined;

function randomID() {
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), x => x.toString(16).padStart(2, '0')).join('');
}

function label(peer: string) { return tr('Device {id}', { id: peer.slice(0, 8).toUpperCase() }); }
function sizeLabel(size: number | null) {
  if (size === null) return tr('Streaming ZIP');
  if (size >= 1e9) return `${(size / 1e9).toFixed(1)} GB`;
  if (size >= 1e6) return `${(size / 1e6).toFixed(1)} MB`;
  return size >= 1000 ? `${(size / 1000).toFixed(1)} kB` : `${size} B`;
}

function status(key: string, t?: Offer) {
  notice.value = { key, name: t?.name, text: t?.content === 'text' };
}

function transmit(data: string | ArrayBuffer) {
  if (!ws || ws.readyState !== WebSocket.OPEN) throw new Error('Connection lost.');
  ws.send(data);
}

function signal(to: string, data: Signal) { transmit(JSON.stringify({ type: 'signal', to, data })); }

function cancelPeer(t: Transfer) {
  if (ws?.readyState === WebSocket.OPEN) signal(t.peer, { kind: 'cancel', id: t.id });
}

function stopSending(reason: string) {
  outgoing.value?.abort.abort(new Error(reason));
  waiter?.finish(new Error(reason));
}

function clearOffer() {
  window.clearTimeout(offerTimer);
  offer.value = undefined;
}

function dropIncoming() {
  window.clearTimeout(incoming.value?.timer);
  incoming.value = undefined;
  progress.value = undefined;
}

function receiveFailed(t: Incoming, reason: string, notify = true) {
  if (notify) cancelPeer(t);
  dropIncoming();
  notice.value = undefined;
  failure.value = reason;
}

function clearRoom(reason: string) {
  if (incoming.value) receiveFailed(incoming.value, reason, false);
  stopSending(reason);
  clearOffer();
  peers.value = [];
  clearSelection();
}

function connect() {
  const url = new URL('/ws', location.href);
  url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const connection = new WebSocket(url);
  ws = connection;
  connection.binaryType = 'arraybuffer';
  connection.onmessage = ({ data }) => {
    if (data instanceof ArrayBuffer) { receiveChunk(data); return; }
    const m = JSON.parse(data);
    const pending = offer.value;
    const active = incoming.value;
    switch (m.type) {
      case 'hello':
        self.value = m.id;
        ready.value = true;
        if (room.value) join(room.value);
        break;
      case 'joined':
        room.value = m.code;
        changing.value = joining.value = false;
        break;
      case 'join-error':
        joining.value = false;
        roomError.value = m.error;
        changing.value = true;
        room.value = '';
        break;
      case 'peers':
        if (room.value !== m.code) break;
        peers.value = (m.peers as string[]).slice().sort();
        if (offer.value && !peers.value.includes(offer.value.peer)) {
          clearOffer();
          failure.value = 'The sender disconnected.';
        }
        if (incoming.value && !peers.value.includes(incoming.value.peer)) receiveFailed(incoming.value, 'The sender disconnected.', false);
        if (outgoing.value && !peers.value.includes(outgoing.value.peer)) stopSending('The recipient disconnected.');
        break;
      case 'signal':
        receiveSignal(m.from, m.data);
        break;
      case 'download-ready':
        if (pending && pending.id === m.id && ID.test(m.token)) pending.token = m.token;
        break;
      case 'download-started':
        if (active && active.id === m.id && !active.content) window.clearTimeout(active.timer);
        break;
      case 'download-finished':
        if (active && active.id === m.id) {
          status('Saved {name}.', active);
          dropIncoming();
        }
        break;
      case 'download-failed':
        if (active && active.id === m.id) receiveFailed(active, 'The download stopped before it finished.', false);
        if (pending && pending.id === m.id) {
          clearOffer();
          failure.value = 'The download could not be started.';
        }
        break;
      case 'error':
        failure.value = m.error;
        break;
    }
  };
  connection.onclose = () => {
    if (stopped) return;
    ready.value = joining.value = false;
    clearRoom('Connection lost. Start the transfer again after reconnecting.');
    reconnect = window.setTimeout(connect, 1500);
  };
}

function join(code: string) {
  roomError.value = '';
  if (!CODE.test(code)) { roomError.value = 'Enter exactly six digits.'; return; }
  joining.value = true;
  transmit(JSON.stringify({ type: 'join', code }));
}

function changeRoom() {
  if (room.value && ready.value) transmit(JSON.stringify({ type: 'leave' }));
  clearRoom('Pairing code changed.');
  room.value = input.value = roomError.value = '';
  changing.value = true;
}

function receiveSignal(peer: string, data: Signal) {
  if (!data || !ID.test(data.id) || !peers.value.includes(peer)) return;
  const { id, kind, name, size, content } = data;
  if (kind === 'offer') {
    if (busy.value || typeof name !== 'string' || name.length < 1 ||
        (size !== null && (typeof size !== 'number' || !Number.isSafeInteger(size) || size < 0 || size > LIMIT)) ||
        (content === 'text' && (size === null || size === 0 || size > TEXT_LIMIT))) {
      cancelPeer({ peer, id });
      return;
    }
    failure.value = '';
    offer.value = { peer, id, name, size, content };
    if (content === 'text') {
      accept();
      return;
    }
    offerTimer = window.setTimeout(() => {
      reject();
      failure.value = 'The transfer timed out.';
    }, 55_000);
    transmit(JSON.stringify({ type: 'prepare-download', to: peer, id }));
    return;
  }
  const send = outgoing.value;
  const recv = incoming.value;
  if (kind === 'cancel') {
    if (send?.id === id && send.peer === peer) stopSending('The recipient declined or cancelled the transfer.');
    if (offer.value?.id === id && offer.value.peer === peer) {
      clearOffer();
      failure.value = 'The sender cancelled the transfer.';
    }
    if (recv?.id === id && recv.peer === peer) receiveFailed(recv, 'The sender cancelled the transfer.', false);
  } else if (send?.id === id && send.peer === peer && waiter?.kind === kind) {
    waiter.finish();
  } else if (kind === 'end' && recv?.id === id && recv.peer === peer && recv.content === 'text') {
    if (recv.received !== recv.size) {
      receiveFailed(recv, 'The transfer ended before all bytes arrived.');
      return;
    }
    receivedText.value = recv.chunks.map((part, i) => decoder.decode(part, { stream: i + 1 < recv.chunks.length })).join('');
    status('Received {name}.', recv);
    dropIncoming();
    signal(peer, { kind: 'done', id });
  }
}

function reject() {
  if (offer.value) cancelPeer(offer.value);
  clearOffer();
}

function receiveTimeout(t: Incoming) {
  return window.setTimeout(() => receiveFailed(t, 'The transfer timed out.'), 30_000);
}

function accept() {
  const t = offer.value;
  if (!t) return;
  clearOffer();
  receivedText.value = '';
  copyMessage.value = '';
  const next: Incoming = { ...t, received: 0, chunks: [] };
  incoming.value = next;
  next.timer = receiveTimeout(next);
  status('Receiving {name}…', t);
  if (t.content === 'text') {
    receivedFrom.value = t.peer;
    progress.value = 0;
    signal(t.peer, { kind: 'accept', id: t.id });
  }
}

function receiveChunk(data: ArrayBuffer) {
  const t = incoming.value;
  if (!t || t.content !== 'text' || !t.size || data.byteLength <= 64 || data.byteLength > CHUNK + 64) return;
  if (decoder.decode(new Uint8Array(data, 0, 64)) !== t.peer + t.id) return;
  const bytes = data.slice(64);
  if (t.received + bytes.byteLength > t.size) {
    receiveFailed(t, 'The received data exceeds the offered size.');
    return;
  }
  window.clearTimeout(t.timer);
  t.timer = receiveTimeout(t);
  t.chunks.push(bytes);
  t.received += bytes.byteLength;
  progress.value = t.received / t.size;
  signal(t.peer, { kind: 'ack', id: t.id });
}

function waitFor(kind: string, send: () => void, timeout = 25_000) {
  return new Promise<void>((resolve, reject) => {
    const timer = window.setTimeout(() => finish(new Error('The transfer timed out.')), timeout);
    const finish = (error?: Error) => {
      window.clearTimeout(timer);
      waiter = undefined;
      if (error) reject(error); else resolve();
    };
    waiter = { kind, finish };
    try { send(); }
    catch (error) { finish(error instanceof Error ? error : new Error(String(error))); }
  });
}

async function send(peer: string, name: string, size: number | null, source: Sender, content?: 'text') {
  const t: Outgoing = { peer, id: randomID(), abort: new AbortController() };
  outgoing.value = t;
  const details = { ...t, name, size, content };
  failure.value = '';
  try {
    notice.value = { key: content === 'text' ? 'Sending to {device}…' : 'Waiting for {device} to accept…', peer: t.peer };
    await waitFor('accept', () => signal(t.peer, { kind: 'offer', id: t.id, name, size, content }), 60_000);
    status('Sending {name}…', details);
    let sent = 0;
    const header = encoder.encode(t.peer + t.id);
    await source(async bytes => {
      for (let i = 0; i < bytes.length; i += CHUNK) {
        t.abort.signal.throwIfAborted();
        const part = bytes.subarray(i, i + CHUNK);
        if (sent + part.length > LIMIT) throw new Error('The archive exceeds 100 GB.');
        const frame = new Uint8Array(header.length + part.length);
        frame.set(header);
        frame.set(part, header.length);
        await waitFor('ack', () => transmit(frame.buffer));
        sent += part.length;
        if (size) progress.value = sent / size;
      }
    }, t.abort.signal);
    t.abort.signal.throwIfAborted();
    await waitFor('done', () => signal(t.peer, { kind: 'end', id: t.id }));
    status('Sent {name}.', details);
  } catch (error) {
    cancelPeer(t);
    failure.value = error instanceof Error ? error.message : String(error);
    notice.value = undefined;
  } finally {
    progress.value = undefined;
    outgoing.value = undefined;
  }
}

function sendFiles(peer: string, files: File[], folder = false) {
  if (files.reduce((sum, file) => sum + file.size, 0) > LIMIT) {
    failure.value = 'The selection exceeds 100 GB.';
    return;
  }
  const first = files[0];
  if (!folder && files.length === 1) {
    void send(peer, first.name, first.size, async (write, abort) => {
      for (let offset = 0; offset < first.size; offset += CHUNK) {
        abort.throwIfAborted();
        await write(new Uint8Array(await first.slice(offset, offset + CHUNK).arrayBuffer()));
      }
    });
    return;
  }
  const name = folder ? `${first.webkitRelativePath.split('/')[0] || 'folder'}.zip` : 'localwebs-files.zip';
  void send(peer, name, null, async (write, abort) => {
    const archive = new ZipWriter(new WritableStream({ write }));
    for (const file of files) {
      abort.throwIfAborted();
      await archive.add((folder ? file.webkitRelativePath : '') || file.name, new BlobReader(file), { level: 0, zip64: true, signal: abort });
    }
    await archive.close();
  });
}

function selectFiles(event: Event, folder: boolean) {
  const element = event.currentTarget as HTMLInputElement;
  const files = Array.from(element.files ?? []);
  element.value = '';
  if (!files.length || busy.value) return;
  clearSelection();
  selection.value = { files, folder };
}

function paste(event: ClipboardEvent) {
  selection.value = undefined;
  const files = Array.from(event.clipboardData?.files ?? []);
  if (files.length) {
    event.preventDefault();
    clipboard.value = '';
  }
  clipboardFiles.value = files;
}

function clearSelection() {
  selection.value = undefined;
  clipboard.value = '';
  clipboardFiles.value = [];
}

function sendTo(peer: string) {
  if (busy.value || peer === self.value || !peers.value.includes(peer)) return;
  if (selection.value) {
    sendFiles(peer, selection.value.files, selection.value.folder);
    return;
  }
  if (clipboardFiles.value.length) {
    sendFiles(peer, clipboardFiles.value);
    return;
  }
  const bytes = encoder.encode(clipboard.value);
  if (bytes.length > TEXT_LIMIT) { failure.value = 'Clipboard text must be 1 MiB or less.'; return; }
  if (bytes.length) void send(peer, 'clipboard.txt', bytes.length, write => write(bytes), 'text');
}

async function copyText() {
  try {
    if (!navigator.clipboard) throw new Error('Clipboard unavailable.');
    await navigator.clipboard.writeText(receivedText.value);
    copyMessage.value = 'Text copied.';
  } catch {
    copyMessage.value = 'Copy is unavailable here. Select and copy the text manually.';
  }
}

function dismissDialog() {
  if (offer.value) reject();
  else if (incoming.value?.content === 'text') receiveFailed(incoming.value, 'Transfer cancelled.');
  else {
    receivedText.value = '';
    copyMessage.value = '';
  }
}

function cancel() {
  if (outgoing.value) stopSending('Transfer cancelled.');
  else if (incoming.value) receiveFailed(incoming.value, 'Transfer cancelled.');
}

onMounted(connect);
onUnmounted(() => {
  stopped = true;
  window.clearTimeout(reconnect);
  ws?.close();
  clearRoom('Connection closed.');
});
</script>

<template>
  <main class="mx-auto max-w-3xl px-5 py-10 sm:py-16">
    <header :inert="changing" class="mb-9 flex flex-wrap items-start justify-between gap-4">
      <div>
        <p class="mb-2 text-xs font-semibold tracking-widest text-cyan-300 uppercase">Local Web Send</p>
        <h1 class="text-4xl font-bold tracking-tight sm:text-5xl">Localwebs</h1>
        <p class="mt-3 text-slate-400">{{ tr('Pair your devices and send files right from the browser.') }}</p>
      </div>
      <div class="flex flex-wrap gap-2">
        <LanguagePicker />
        <a href="/admin" class="secondary">{{ tr('Admin') }}</a>
      </div>
    </header>

    <section :inert="changing" class="card grid gap-5">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 class="text-lg font-semibold">{{ tr('Your pairing room') }}</h2>
          <p v-if="room" class="mt-1 font-mono text-2xl tracking-widest text-cyan-300">{{ room }}</p>
          <p v-else class="muted mt-1">{{ tr('Not paired yet') }}</p>
        </div>
        <button v-if="room" class="secondary" @click="changeRoom">{{ tr('Change code') }}</button>
      </div>
      <p v-if="!ready" class="muted" role="status">{{ tr('Connecting to the server…') }}</p>
      <div v-if="ready && room" class="grid gap-3">
        <div class="flex flex-wrap gap-2">
          <button class="secondary" :disabled="busy" @click="fileInput?.click()">{{ tr('Choose files') }}</button>
          <button class="secondary" :disabled="busy" @click="folderInput?.click()">{{ tr('Choose folder') }}</button>
          <input ref="fileInput" type="file" multiple hidden @change="selectFiles($event, false)">
          <input ref="folderInput" type="file" webkitdirectory multiple hidden @change="selectFiles($event, true)">
        </div>
        <p class="muted">{{ tr('Up to 100 GB per transfer. Folders and multiple files arrive as a ZIP.') }}</p>
        <label class="field">
          {{ tr('Clipboard') }}
          <textarea v-model="clipboard" rows="3" :disabled="busy" :placeholder="tr('Paste text, a link, an image, or files here')"
            @paste="paste" @input="clipboardFiles = []; selection = undefined" />
        </label>
        <p v-if="selection?.folder" class="muted break-all">{{ selection.files[0].webkitRelativePath.split('/')[0] }}</p>
        <ul v-else-if="selection || clipboardFiles.length" class="muted max-h-36 list-inside list-disc overflow-auto break-all">
          <li v-for="(file, index) in (selection?.files ?? clipboardFiles)" :key="index">{{ file.name }}</li>
        </ul>
        <button v-if="selection || clipboard || clipboardFiles.length" class="secondary justify-self-start"
          :disabled="busy" @click="clearSelection">{{ tr('Clear selection') }}</button>
      </div>
    </section>

    <section v-if="room && ready" :inert="changing" class="card mt-4 grid gap-3" aria-labelledby="devices-title">
      <h2 id="devices-title" class="text-lg font-semibold">{{ tr('Devices in this room') }}</h2>
      <p class="muted">{{ tr('Choose content, then click the receiving device.') }}</p>
      <ul class="grid gap-2">
        <li v-if="self" class="flex flex-wrap items-center gap-2 rounded-xl border border-slate-700 px-4 py-3">
          <span>{{ label(self) }}</span>
          <span class="rounded-full bg-slate-800 px-2 py-0.5 text-xs text-cyan-300">{{ tr('This device') }}</span>
        </li>
        <li v-for="peer in peers" :key="peer">
          <button class="secondary w-full justify-start text-left"
            :disabled="busy || (!selection && !clipboard && !clipboardFiles.length)" @click="sendTo(peer)">{{ label(peer) }}</button>
        </li>
      </ul>
      <p v-if="!peers.length" class="muted" role="status">{{ tr('Enter this code on another device to connect.') }}</p>
    </section>

    <section v-if="notice || failure" :inert="changing" class="card mt-4 grid gap-3" aria-live="polite">
      <p v-if="notice" class="break-all">{{ tr(notice.key, { name: notice.text ? tr('Clipboard text') : notice.name ?? '', device: notice.peer ? label(notice.peer) : '' }) }}</p>
      <p v-if="failure" class="error" role="alert">{{ tr(failure) }}</p>
      <progress v-if="progress !== undefined" :value="progress" max="1" :aria-label="tr('Transfer progress')" />
      <button v-if="outgoing || incoming" class="secondary justify-self-start" @click="cancel">{{ tr('Cancel transfer') }}</button>
    </section>

    <dialog ref="offerDialog" class="card m-auto max-h-[calc(100dvh-2rem)] w-[calc(100%-2.5rem)] max-w-md overflow-y-auto text-slate-100 backdrop:bg-slate-950/85 backdrop:backdrop-blur-sm"
      aria-labelledby="offer-title" :aria-describedby="offer ? 'offer-details' : undefined" @cancel.prevent="dismissDialog" @keydown.esc.prevent.stop="dismissDialog">
      <template v-if="offer">
        <h2 id="offer-title" class="text-lg font-semibold">{{ tr('{device} wants to send you a file', { device: label(offer.peer) }) }}</h2>
        <p id="offer-details" class="mt-3 break-all">{{ offer.name }} · {{ sizeLabel(offer.size) }}</p>
        <div class="mt-5 flex flex-wrap gap-2">
          <a v-if="offer.token" :href="`/download/${offer.token}`" :download="offer.name" class="primary" @click="accept">{{ tr('Accept and download') }}</a>
          <button v-else class="primary" disabled>{{ tr('Preparing download…') }}</button>
          <button class="secondary" autofocus @click="reject">{{ tr('Decline') }}</button>
        </div>
      </template>
      <template v-else-if="textDialogOpen">
        <h2 id="offer-title" class="text-lg font-semibold">{{ tr('Clipboard text from {device}', { device: label(receivedFrom) }) }}</h2>
        <div id="offer-details" class="mt-3 grid gap-3">
          <template v-if="receivedText">
            <textarea :value="receivedText" :aria-label="tr('Clipboard text')" rows="6" readonly />
            <p v-if="copyMessage" :class="copyMessage === 'Text copied.' ? 'muted' : 'error'" role="status">{{ tr(copyMessage) }}</p>
            <div class="flex flex-wrap gap-2">
              <button class="primary" @click="copyText">{{ tr('Copy text') }}</button>
              <button class="secondary" @click="dismissDialog">{{ tr('Close') }}</button>
            </div>
          </template>
          <template v-else>
            <p class="muted">{{ tr('Receiving {name}…', { name: tr('Clipboard text') }) }}</p>
            <progress v-if="progress !== undefined" :value="progress" max="1" :aria-label="tr('Transfer progress')" />
            <button class="secondary justify-self-start" @click="dismissDialog">{{ tr('Cancel transfer') }}</button>
          </template>
        </div>
      </template>
    </dialog>

    <div v-if="changing" class="fixed inset-0 grid place-items-center overflow-y-auto bg-slate-950/85 px-5 py-8 backdrop-blur-sm">
      <form class="card w-full max-w-sm" role="dialog" aria-modal="true" aria-labelledby="pair-title" @submit.prevent="join(input)">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <h2 id="pair-title" class="text-2xl font-semibold">{{ tr('Pair your devices') }}</h2>
          <LanguagePicker />
        </div>
        <p class="muted mt-2">{{ tr('Enter the same six-digit code on every device you want to connect.') }}</p>
        <label class="field mt-5">
          {{ tr('Pairing code') }}
          <input v-model="input" type="text" inputmode="numeric" autocomplete="off" maxlength="6" placeholder="000000"
            class="font-mono text-xl tracking-widest" autofocus>
        </label>
        <p v-if="roomError" class="error mt-2" role="alert">{{ tr(roomError) }}</p>
        <button class="primary mt-5 w-full" :disabled="!ready || joining">{{ tr(joining ? 'Joining…' : ready ? 'Join room' : 'Connecting…') }}</button>
        <a href="/admin" class="muted mt-4 block text-center">{{ tr('Administration') }}</a>
      </form>
    </div>
  </main>
</template>
