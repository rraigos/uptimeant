module.exports = {
  apps: [
    {
      name: 'pulsecheck',
      script: './pulsecheck',
      interpreter: 'none',
      instances: 1,
      exec_mode: 'fork',
      autorestart: true,
    },
  ],
};
