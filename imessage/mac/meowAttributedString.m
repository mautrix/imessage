// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2022 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

#include <Foundation/Foundation.h>
#include "meowAttributedString.h"

char* meowDecodeAttributedString(char* input) {
    NSString* nsInput = @(input);
    NSData* data = [[NSData alloc] initWithBase64EncodedString:nsInput options:0];
    NSUnarchiver* arch = [[NSUnarchiver alloc] initForReadingWithData:data];
    NSAttributedString* str = [arch decodeObject];

    NSMutableArray* attrs = [[NSMutableArray alloc] init];
    [str enumerateAttributesInRange:NSMakeRange(0, [str length]) options:NSAttributedStringEnumerationLongestEffectiveRangeNotRequired usingBlock:
        ^(NSDictionary *attributes, NSRange range, BOOL *stop) {
            [attrs addObject:@{
                @"location": [NSNumber numberWithUnsignedInteger:range.location],
                @"length": [NSNumber numberWithUnsignedInteger:range.length],
                @"values": attributes,
            }];
        }
    ];
    NSDictionary* outputDict = @{
        @"content": str.string,
        @"attributes": attrs,
    };

    NSError *error = NULL;
    NSData *jsonData = [NSJSONSerialization dataWithJSONObject:outputDict options:0 error:&error];
    if (!jsonData && error) {
        NSString* fancyError = [[error.localizedDescription stringByAppendingString:@". "] stringByAppendingString:error.localizedFailureReason];
        return [fancyError UTF8String];
    }
    NSString *jsonString = [[NSString alloc] initWithData:jsonData encoding:NSUTF8StringEncoding];
    return [jsonString UTF8String];
}
