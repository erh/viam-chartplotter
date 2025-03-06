import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

const config = {
	preprocess: vitePreprocess(),
    kit: {
        paths: {
            base: process.env.NODE_ENV === 'production' ? '/viam-chartplotter' : '',
        }
        
    }
};

export default config;
