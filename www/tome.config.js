/** @type {import('@tomehq/core').TomeConfig} */
export default {
  name: "BONNIE",
  logo: "/logo.png",
  favicon: "/favicon.svg",
  baseUrl: "https://go-bonnie.dev",
  theme: {
    preset: "cipher",
    accent: "#00cbea",
    mode: "dark",
    fonts: {
      heading: "Space Grotesk",
      body: "Space Grotesk",
      code: "Source Code Pro",
    },
  },
  navigation: [
    { group: "Start here", pages: ["index", "installation", "quick-start", "concepts"] },
    { group: "Build an agent", pages: ["guides/agent-trees", "guides/agent-commands", "guides/tools-and-skills", "guides/sandboxes", "guides/scheduling"] },
    { group: "Connect channels", pages: ["channels/overview", "channels/http", "channels/slack", "channels/discord", "channels/telegram", "channels/github", "channels/nats"] },
    { group: "Operate safely", pages: ["guides/deployment", "guides/troubleshooting"] },
    { group: "Reference", pages: ["reference/cli", "reference/options", "reference/runtime", "reference/journal", "reference/events", "reference/testing"] },
  ],
  socialLinks: [
    { platform: "github", url: "https://github.com/mark3labs/bonnie" },
  ],
};
