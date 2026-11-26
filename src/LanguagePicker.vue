<script setup lang="ts">
import { language, tr } from './i18n';
</script>

<template>
  <select v-model="language" :aria-label="tr('Language')" class="secondary bg-slate-950">
    <option value="en" lang="en">English</option>
    <option value="zh-CN" lang="zh-CN">简体中文</option>
  </select>
</template>
