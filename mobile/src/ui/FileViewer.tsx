import { Feather } from '@expo/vector-icons';
import { useEffect, useState } from 'react';
import { ActivityIndicator, Image, Pressable, ScrollView, StyleSheet, Text, View } from 'react-native';
import { api, type Pairing } from '../api/client';
import type { AttachmentRef } from '../api/types';
import { useTheme } from '../theme/ThemeContext';

/**
 * 附件阅读器：手机上能看的，只有 agent 在这段对话里用 SendFile 交出去的那几个。
 *
 * 取文件走带令牌的下载端点，端点只认交付记录里的 id（不接受路径）——
 * 所以这里拿到的永远是"这段对话真的交付过"的东西。
 */
export function FileViewer({
  pairing,
  sessionId,
  file,
  onClose,
}: {
  pairing: Pairing;
  sessionId: string;
  file: AttachmentRef;
  onClose: () => void;
}) {
  const { tokens: t } = useTheme();
  const isImage = file.mimeType.startsWith('image/');
  const isText = file.mimeType.startsWith('text/') || file.mimeType === 'application/json';
  const [text, setText] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);
  const [loading, setLoading] = useState(isText);

  const sizeText =
    file.size > 1024 * 1024 ? `${(file.size / 1048576).toFixed(1)} MB` : `${Math.round(file.size / 1024)} KB`;

  useEffect(() => {
    if (!isText) return;
    let alive = true;
    const { url, headers } = api.file(pairing, sessionId, file);
    fetch(url, { headers })
      .then((r) => {
        if (!r.ok) throw new Error(String(r.status));
        return r.text();
      })
      .then((body) => {
        if (alive) setText(body);
      })
      .catch(() => {
        if (alive) setFailed(true);
      })
      .finally(() => {
        if (alive) setLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [pairing, sessionId, file, isText]);

  const remote = api.file(pairing, sessionId, file);

  return (
    <View style={[styles.root, { backgroundColor: t.bg }]}>
      <View style={[styles.header, { borderBottomColor: t.border, backgroundColor: t.bgTop }]}>
        <Pressable onPress={onClose} style={({ pressed }) => [styles.back, pressed && { backgroundColor: t.wash }]}>
          <Feather name="chevron-left" size={22} color={t.dim} />
        </Pressable>
        <View style={styles.headInfo}>
          <Text style={[styles.title, { color: t.title }]} numberOfLines={1}>
            {file.name}
          </Text>
          <Text style={{ color: t.faint, fontSize: 11.5, marginTop: 2 }}>
            {isImage ? '图片' : isText ? '文本' : '其他'} · {sizeText}
          </Text>
        </View>
      </View>

      {isImage ? (
        <ScrollView contentContainerStyle={styles.imageWrap} maximumZoomScale={3}>
          <Image
            source={{ uri: remote.url, headers: remote.headers }}
            style={[styles.image, { borderColor: t.border, backgroundColor: t.panel }]}
            resizeMode="contain"
          />
        </ScrollView>
      ) : isText ? (
        <ScrollView contentContainerStyle={styles.textWrap}>
          {loading ? (
            <ActivityIndicator color={t.faint} style={{ marginTop: 30 }} />
          ) : failed ? (
            <Text style={{ color: t.red, fontSize: 13 }}>取不到这个文件了。</Text>
          ) : (
            <Text style={[styles.doc, { color: t.text, backgroundColor: t.panel, borderColor: t.border }]}>
              {text}
            </Text>
          )}
        </ScrollView>
      ) : (
        <View style={styles.notice}>
          <Feather name="external-link" size={22} color={t.faint} />
          <Text style={{ color: t.faint, fontSize: 13, marginTop: 10, textAlign: 'center' }}>
            这种文件手机打不开，在电脑上打开。
          </Text>
        </View>
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  root: { flex: 1 },
  header: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 6,
    height: 58,
    paddingHorizontal: 8,
    borderBottomWidth: StyleSheet.hairlineWidth,
  },
  back: { width: 34, height: 34, borderRadius: 9, alignItems: 'center', justifyContent: 'center' },
  headInfo: { flex: 1, minWidth: 0, paddingHorizontal: 4 },
  title: { fontSize: 14.5, fontWeight: '600' },
  imageWrap: { padding: 16 },
  image: { width: '100%', height: 420, borderWidth: 1, borderRadius: 12 },
  textWrap: { padding: 16 },
  doc: {
    borderWidth: 1,
    borderRadius: 12,
    padding: 14,
    fontSize: 13,
    lineHeight: 21,
    fontFamily: 'monospace',
  },
  notice: { flex: 1, alignItems: 'center', justifyContent: 'center', padding: 30 },
});
