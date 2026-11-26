import { createApp } from 'vue';
import Admin from './Admin.vue';
import Transfer from './Transfer.vue';
import './style.css';

createApp(location.pathname === '/admin' ? Admin : Transfer).mount('#app');
