import { createApp } from "vue";
import { createPinia } from "pinia";
import App from "./App.vue";
import router from "./router.js";
import { useThemeStore } from "./store/theme.js";
import { autoScrollbar } from "./lib/autoScrollbar.js";
import { onSessionEnd } from "./auth/oidc.js";
import { useUserStore } from "./store/user.js";

const pinia = createPinia();
const app = createApp(App);
app.use(pinia);
useThemeStore(pinia);
// L-26: an expired, revoked or signed-out session takes its chat history along.
onSessionEnd(() => useUserStore(pinia).clear());
app.use(router);
app.directive("autoscroll", autoScrollbar);
app.mount("#app");
