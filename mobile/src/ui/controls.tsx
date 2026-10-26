import { Pressable, StyleSheet, View } from 'react-native';
import type { useTheme } from '../theme/ThemeContext';

/**
 * 开关。规格只有一份：会话信息页（审批、保持唤醒）和设置页画的是同一个东西 ——
 * 两处各写一个的话，圆角、滑块位置和禁用态迟早会分叉。
 */
export function Switch({
  on,
  disabled,
  onPress,
  t,
}: {
  on: boolean;
  disabled?: boolean;
  onPress: () => void;
  t: ReturnType<typeof useTheme>['tokens'];
}) {
  return (
    <Pressable
      disabled={disabled}
      onPress={onPress}
      style={[
        styles.switch,
        {
          backgroundColor: on ? t.amberWash : t.washStrong,
          borderColor: on ? t.amberLine : t.borderStrong,
          opacity: disabled ? 0.4 : 1,
        },
      ]}
    >
      <View style={[styles.knob, { left: on ? 20 : 2, backgroundColor: on ? t.amber : t.switchOff }]} />
    </Pressable>
  );
}

const styles = StyleSheet.create({
  switch: { width: 44, height: 26, borderRadius: 20, borderWidth: 1, justifyContent: 'center' },
  knob: { position: 'absolute', width: 20, height: 20, borderRadius: 10 },
});
