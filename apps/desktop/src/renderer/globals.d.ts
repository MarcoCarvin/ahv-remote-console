// Side-effect CSS imports: tsc has no built-in knowledge of them, so we
// declare them as opaque modules. Vite handles the actual asset graph.
declare module '*.css';
declare module '*.svg';
declare module '*.png';
declare module '*.jpg';
declare module '*.webp';
