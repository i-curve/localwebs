<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue';
import LanguagePicker from './LanguagePicker.vue';
import { tr } from './i18n';

type Settings = {
  username: string;
  clusterEnabled: boolean;
  clusterPeerUrls: string[];
  hasClusterKey: boolean;
  clusterConnectedPeers: number;
};

class APIError extends Error {
  constructor(message: string, readonly status: number) { super(message); }
}

const authenticated = ref(false);
const loading = ref(true);
const busy = ref(false);
const error = ref('');
const status = ref('');
const username = ref('');
const password = ref('');
const adminUsername = ref('');
const adminPassword = ref('');
const clusterEnabled = ref(false);
const clusterPeerUrls = ref('');
const clusterKey = ref('');
const hasClusterKey = ref(false);
const connected = ref(0);
const enabled = ref(false);
let polling: number | undefined;

async function api(path: string, method = 'GET', body?: object): Promise<Response> {
  const response = await fetch(`/api/admin/${path}`, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined
  });
  if (!response.ok) {
    const data = await response.json();
    throw new APIError(data.error, response.status);
  }
  return response;
}

async function settings(method = 'GET', body?: object): Promise<Settings> {
  const response = await api('settings', method, body);
  return response.json();
}

function useSettings(s: Settings) {
  adminUsername.value = s.username;
  clusterEnabled.value = enabled.value = s.clusterEnabled;
  clusterPeerUrls.value = s.clusterPeerUrls.join('\n');
  hasClusterKey.value = s.hasClusterKey;
  connected.value = s.clusterConnectedPeers;
}

function showError(e: unknown) {
  error.value = e instanceof Error ? e.message : String(e);
  if (authenticated.value && e instanceof APIError && e.status === 401) authenticated.value = false;
}

async function action(work: () => Promise<void>) {
  busy.value = true;
  error.value = status.value = '';
  try { await work(); }
  catch (e) { showError(e); }
  finally { busy.value = false; }
}

async function login() {
  await action(async () => {
    await api('login', 'POST', { username: username.value, password: password.value });
    password.value = '';
    useSettings(await settings());
    authenticated.value = true;
  });
}

async function logout() {
  await action(async () => {
    await api('logout', 'POST', {});
    authenticated.value = false;
    username.value = adminPassword.value = clusterKey.value = '';
  });
}

async function save() {
  await action(async () => {
    useSettings(await settings('PUT', {
      username: adminUsername.value,
      password: adminPassword.value || undefined,
      clusterEnabled: clusterEnabled.value,
      clusterPeerUrls: clusterPeerUrls.value.split(/\r?\n/).map(url => url.trim()).filter(Boolean),
      clusterKey: clusterKey.value || undefined
    }));
    adminPassword.value = clusterKey.value = '';
    status.value = 'Settings saved.';
  });
}

onMounted(async () => {
  try {
    useSettings(await settings());
    authenticated.value = true;
  } catch (e) {
    if (!(e instanceof APIError) || e.status !== 401) showError(e);
  } finally { loading.value = false; }
  polling = window.setInterval(async () => {
    if (!authenticated.value || busy.value || document.hidden) return;
    try {
      const s = await settings();
      if (!authenticated.value || busy.value) return;
      enabled.value = s.clusterEnabled;
      connected.value = s.clusterConnectedPeers;
    } catch (e) {
      if (authenticated.value && !busy.value) showError(e);
    }
  }, 3000);
});
onUnmounted(() => window.clearInterval(polling));
</script>

<template>
  <main class="mx-auto max-w-2xl px-5 py-10 sm:py-16">
    <header class="mb-7 flex flex-wrap items-start justify-between gap-4">
      <div>
        <p class="mb-2 text-xs font-semibold tracking-widest text-cyan-300 uppercase">Localwebs</p>
        <h1 class="text-4xl font-bold tracking-tight">{{ tr('Administration') }}</h1>
      </div>
      <div class="flex flex-wrap gap-2">
        <LanguagePicker />
        <a href="/" class="secondary">{{ tr('Back to transfers') }}</a>
      </div>
    </header>
    <div v-if="error || status" class="card mb-4" aria-live="polite">
      <p v-if="error" class="error" role="alert">{{ tr(error) }}</p>
      <p v-else class="text-sm text-cyan-300">{{ tr(status) }}</p>
    </div>
    <p v-if="loading" class="muted" role="status">{{ tr('Loading…') }}</p>
    <form v-else-if="!authenticated" class="card grid gap-4" @submit.prevent="login">
      <h2 class="text-xl font-semibold">{{ tr('Sign in') }}</h2>
      <label class="field">{{ tr('Username') }} <input v-model="username" autocomplete="username" required></label>
      <label class="field">{{ tr('Password') }} <input v-model="password" type="password" autocomplete="current-password" required></label>
      <button class="primary justify-self-start" :disabled="busy">{{ tr(busy ? 'Signing in…' : 'Sign in') }}</button>
    </form>

    <form v-else class="card grid gap-5" @submit.prevent="save">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <h2 class="text-xl font-semibold">{{ tr('Administrator account') }}</h2>
        <button type="button" class="secondary" :disabled="busy" @click="logout">{{ tr('Sign out') }}</button>
      </div>
      <label class="field">{{ tr('Username') }} <input v-model="adminUsername" autocomplete="username" required maxlength="128"></label>
      <label class="field">{{ tr('New password') }}
        <input v-model="adminPassword" type="password" minlength="12" maxlength="1024" autocomplete="new-password" :placeholder="tr('Leave blank to keep current password')">
      </label>

      <div class="flex flex-wrap items-center justify-between gap-3 border-t border-slate-800 pt-5">
        <h2 class="text-xl font-semibold">{{ tr('Cluster') }}</h2>
        <span class="rounded-full bg-slate-800 px-3 py-1 text-sm" role="status">{{ tr(enabled ? connected ? 'Connected to {count} direct peer(s)' : 'Waiting for peers' : 'Disabled', { count: connected }) }}</span>
      </div>
      <label class="flex items-center gap-2"><input v-model="clusterEnabled" type="checkbox" class="accent-cyan-300"> {{ tr('Enable cluster') }}</label>
      <label class="field">{{ tr('Peer URLs (one per line)') }}
        <textarea v-model="clusterPeerUrls" rows="3" placeholder="https://peer.example.com" spellcheck="false" />
      </label>
      <label class="field">{{ tr('Shared key') }}
        <input v-model="clusterKey" type="password" minlength="24" maxlength="256" autocomplete="off"
          :placeholder="tr(hasClusterKey ? 'Leave blank to keep current key' : 'At least 24 characters')">
      </label>
      <button class="primary justify-self-start" :disabled="busy">{{ tr(busy ? 'Working…' : 'Save settings') }}</button>
    </form>
  </main>
</template>
