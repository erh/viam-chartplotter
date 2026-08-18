import { mount } from "svelte";
import "./output.css";
import App from "./App.svelte";
import TestApp from "./TestApp.svelte";

// Touchpad pinch arrives as ctrl+wheel; over the map OL handles (and
// preventDefaults) it, but anywhere else the browser would zoom the whole
// page, oversizing the app and bringing back scroll bars. Block it globally —
// OL's own handler runs first on the map viewport, so this doesn't interfere.
document.addEventListener("wheel", (e) => e.ctrlKey && e.preventDefault(), { passive: false });

// Check for ?test URL parameter to load test view
const urlParams = new URLSearchParams(window.location.search);
const isTestMode = urlParams.has("test");

const app = mount(isTestMode ? TestApp : App, {
  target: document.getElementById("app")!,
});

export default app;
