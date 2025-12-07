import { mount } from 'svelte'
import './output.css'
import App from './App.svelte'
import TestApp from './TestApp.svelte'

// Check for ?test URL parameter to load test view
const urlParams = new URLSearchParams(window.location.search)
const isTestMode = urlParams.has('test')

const app = mount(isTestMode ? TestApp : App, {
  target: document.getElementById('app')!,
})

export default app
