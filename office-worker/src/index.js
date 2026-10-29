import './handlers/index.js';
import { assertTokenConfigured, createServer } from './server.js';

assertTokenConfigured();
const port = Number(process.env.PORT) || 8081;
createServer().listen(port, () => {
  console.log(`office-worker listening on :${port}`);
});
