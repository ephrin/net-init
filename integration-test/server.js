// integration-test/server.js
const http = require('http');

const PORT = 8000;
// Read the environment variable passed through net-init/compose
const testVar = process.env.TEST_VAR || 'NOT_SET';

const server = http.createServer((req, res) => {
  console.log(`Received request: ${req.method} ${req.url}`);
  res.writeHead(200, { 'Content-Type': 'text/plain' });
  // Respond with the value of the env var
  res.end(`Hello from Node! TEST_VAR is: ${testVar}\n`);
});

server.listen(PORT, () => {
  // Log that the actual application has started
  console.log(`Node.js echo server started on port ${PORT}`);
  console.log(`TEST_VAR environment variable: ${testVar}`);
});

// Basic signal handling (optional but good practice)
process.on('SIGTERM', () => {
  console.log('SIGTERM signal received: closing HTTP server');
  server.close(() => {
    console.log('HTTP server closed');
    process.exit(0);
  });
});

process.on('SIGINT', () => {
  console.log('SIGINT signal received: closing HTTP server');
  server.close(() => {
    console.log('HTTP server closed');
    process.exit(0);
  });
});