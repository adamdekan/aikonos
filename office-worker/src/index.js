import './handlers/index.js';
import { createServer } from './server.js';

const port = Number(process.env.PORT) || 8081;
createServer().listen(port, () => {
  console.log(`office-worker listening on :${port}`);
});
