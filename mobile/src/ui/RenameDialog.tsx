import { useState } from 'react';
import { Modal, Pressable, StyleSheet, Text, TextInput, View } from 'react-native';
import { useTheme } from '../theme/ThemeContext';

/**
 * 改一段对话的名字。**会话列表里长按一行**和**对话顶栏长按标题**打开的是同一个对话框 ——
 * 两处各写一个的话，「草稿没按保存之前那台电脑上的标题一个字都不动」这条规矩迟早会在其中
 * 一边失守。
 *
 * 草稿留在这里：调用方只在打开的那一刻把它挂上来（`{target ? <RenameDialog .../> : null}`），
 * 所以换一段对话就是一个新组件、草稿自然从它的标题开始，不需要 useEffect 去对齐。
 */
export function RenameDialog({
  initial,
  onCancel,
  onSave,
}: {
  /** 打开那一刻的标题（取消或不改都不回传） */
  initial: string;
  onCancel: () => void;
  onSave: (title: string) => void;
}) {
  const { tokens: t } = useTheme();
  const [draft, setDraft] = useState(initial);

  // 空的、没改的都不发请求：那台电脑上的记录不该因为点了一次「保存」而被重写一遍
  const commit = () => {
    const title = draft.trim();
    if (!title || title === initial) {
      onCancel();
      return;
    }
    onSave(title);
  };

  return (
    <Modal visible transparent animationType="fade" onRequestClose={onCancel}>
      <Pressable style={styles.backdrop} onPress={onCancel}>
        <View style={[styles.dialog, { backgroundColor: t.panel, borderColor: t.border }]}>
          <Text style={[styles.dialogTitle, { color: t.text }]}>重命名</Text>
          <TextInput
            value={draft}
            onChangeText={setDraft}
            autoFocus
            selectTextOnFocus
            placeholder="这段对话叫什么"
            placeholderTextColor={t.faint}
            style={[styles.dialogInput, { color: t.text, borderColor: t.border, backgroundColor: t.inputBg }]}
            // 键盘上的回车就是「保存」：这一屏没有第二件事要用它
            submitBehavior="submit"
            onSubmitEditing={commit}
          />
          <View style={styles.dialogRow}>
            <Pressable onPress={onCancel} hitSlop={8} accessibilityRole="button">
              <Text style={{ color: t.faint, fontSize: 14 }}>取消</Text>
            </Pressable>
            <Pressable onPress={commit} hitSlop={8} accessibilityRole="button">
              <Text style={{ color: t.accent2, fontSize: 14, fontWeight: '600' }}>保存</Text>
            </Pressable>
          </View>
        </View>
      </Pressable>
    </Modal>
  );
}

const styles = StyleSheet.create({
  backdrop: { flex: 1, backgroundColor: 'rgba(0,0,0,0.35)', alignItems: 'center', justifyContent: 'center', padding: 24 },
  dialog: { width: '100%', maxWidth: 420, borderRadius: 14, borderWidth: 1, padding: 16, gap: 12 },
  dialogTitle: { fontSize: 15, fontWeight: '600' },
  dialogInput: { borderWidth: 1, borderRadius: 10, paddingHorizontal: 12, paddingVertical: 10, fontSize: 15 },
  dialogRow: { flexDirection: 'row', justifyContent: 'flex-end', gap: 20, paddingTop: 2 },
});
