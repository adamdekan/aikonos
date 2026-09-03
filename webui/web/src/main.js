import { createApp } from "vue";
import { createPinia } from "pinia";
import App from "./App.vue";
import router from "./router.js";
import { useThemeStore } from "./store/theme.js";
import { autoScrollbar } from "./lib/autoScrollbar.js";

const pinia = createPinia();
const app = createApp(App);
app.use(pinia);
useThemeStore(pinia);
app.use(router);
app.directive("autoscroll", autoScrollbar);
app.mount("#app");
