import React, {useRef} from "react";
import {EntryItem} from "./EntryListItem/EntryListItem";
import styles from './style/EntriesList.module.sass';
import ScrollableFeedVirtualized from "react-scrollable-feed-virtualized";
import down from "./assets/downImg.svg";

interface EntriesListProps {
    entries: any[];
    listEntryREF: any;
    focusedEntryId: string;
    setFocusedEntryId: (id: string) => void;
    updateQuery: any;
    onSnapBrokenEvent: () => void;
    isSnappedToBottom: boolean;
    setIsSnappedToBottom: any;
    queriedCurrent: number;
    queriedTotal: number;
    startTime: number;
}

export const EntriesList: React.FC<EntriesListProps> = ({entries, listEntryREF, focusedEntryId, setFocusedEntryId, updateQuery, onSnapBrokenEvent, isSnappedToBottom, setIsSnappedToBottom, queriedCurrent, queriedTotal, startTime}) => {

    const scrollableRef = useRef(null);

    return <>
            <div className={styles.list}>
                <div id="list" ref={listEntryREF} className={styles.list}>
                    <ScrollableFeedVirtualized ref={scrollableRef} itemHeight={48} marginTop={10} onSnapBroken={onSnapBrokenEvent}>
                        {false /* TODO: why there is a need for something here (not necessarily false)? */}
                        {entries.map(entry => <EntryItem key={entry.id}
                                                        entry={entry}
                                                        setFocusedEntryId={setFocusedEntryId}
                                                        isSelected={focusedEntryId === entry.id.toString()}
                                                        style={{}}
                                                        updateQuery={updateQuery}/>)}
                    </ScrollableFeedVirtualized>
                    <button type="button"
                        className={`${styles.btnLive} ${isSnappedToBottom ? styles.hideButton : styles.showButton}`}
                        onClick={(_) => {
                            scrollableRef.current.jumpToBottom();
                            setIsSnappedToBottom(true);
                        }}>
                        <img alt="down" src={down} />
                    </button>
                </div>

                <div className={styles.footer}>
                    <div>Displaying <b>{entries?.length}</b> results (queried <b>{queriedCurrent}</b>/<b>{queriedTotal}</b>)</div>
                    {startTime !== 0 && <div>Started listening at <span style={{marginRight: 5, fontWeight: 600, fontSize: 13}}>{new Date(startTime).toLocaleString()}</span></div>}
                </div>
            </div>
    </>;
};
