/** Classic command chat content, including Markdown escaping for the shrug's arms. */
export const CLASSIC_COMMAND_PAYLOADS = Object.freeze({
  shrug: '¯\\\\\\_(ツ)\\_/¯',
  tableflip: '(╯°□°)╯︵ ┻━┻',
  unflip: '┬─┬ ノ( ゜-゜ノ)',
});

export function formatClassicCommandMessage(command: keyof typeof CLASSIC_COMMAND_PAYLOADS, text?: string): string {
  return CLASSIC_COMMAND_PAYLOADS[command] + (text ? ' ' + text : '');
}
