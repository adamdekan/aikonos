// Vue directive `v-autoscroll`: reveal a minimal scrollbar only while the
// element is actively scrolled, then fade it out after a short idle. Pairs with
// the `.scroll-min` CSS in styles/tokens.css — apply both together:
//   <div class="scroll-min" v-autoscroll>…</div>
// The directive owns no styling; it only toggles the `is-scrolling` class that
// the CSS reacts to.
const IDLE_MS = 700;

export const autoScrollbar = {
  mounted(el) {
    let timer = null;
    const onScroll = () => {
      el.classList.add("is-scrolling");
      if (timer) clearTimeout(timer);
      timer = setTimeout(() => el.classList.remove("is-scrolling"), IDLE_MS);
    };
    el.__autoScrollbar = { onScroll, clearTimer: () => timer && clearTimeout(timer) };
    el.addEventListener("scroll", onScroll, { passive: true });
  },
  unmounted(el) {
    const handle = el.__autoScrollbar;
    if (handle) {
      el.removeEventListener("scroll", handle.onScroll);
      handle.clearTimer();
      delete el.__autoScrollbar;
    }
  },
};
