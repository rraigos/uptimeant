module.exports = {
  apps: [
    {
      name: 'uptimeant',
      script: './uptimeant',
      interpreter: 'none',
      instances: 1,
      exec_mode: 'fork',
      autorestart: true,
    },
  ],
};
